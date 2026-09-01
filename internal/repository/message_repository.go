package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go_voicemail_service_document_flow/internal/model"
)

var ErrNotFound = errors.New("обращение не найдено")

type MessageRepository struct {
	db *sql.DB
}

func NewMessageRepository(db *sql.DB) *MessageRepository {
	return &MessageRepository{db: db}
}

// Filter — параметры списка. Нулевые поля означают «без фильтра».
type Filter struct {
	Page    int
	Limit   int
	Status  string
	Mailbox string
	From    time.Time
	To      time.Time
}

const messageColumns = `
	id, record_id, mailbox, mailbox_name, caller_number, caller_name,
	received_at, duration_sec, audio_path, audio_bytes, audio_error,
	status, admin_comment, updated_by_id, updated_by_name, updated_at,
	fetched_at, acked_at`

// Upsert сохраняет обращение, забранное с АТС.
//
// Повторный приезд той же записи — норма, а не сбой: пока мы не подтвердили её
// через ack, vmapi отдаёт её каждую ночь заново. Поэтому конфликт не ошибка, и
// разрешается он ровно одним способом — дописать недостающий звук.
//
// Статус и комментарий администратора не трогаются никогда: DO UPDATE ограничен
// условием audio_path IS NULL, то есть срабатывает только у записи, которой в
// прошлый раз не досталось mp3.
func (r *MessageRepository) Upsert(ctx context.Context, m *model.Message) error {
	const query = `
		INSERT INTO messages (
			record_id, mailbox, mailbox_name, caller_number, caller_name,
			received_at, duration_sec, audio_path, audio_bytes, audio_error, fetched_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (mailbox, record_id) DO UPDATE SET
			audio_path  = excluded.audio_path,
			audio_bytes = excluded.audio_bytes,
			audio_error = excluded.audio_error
		WHERE messages.audio_path IS NULL`

	_, err := r.db.ExecContext(ctx, query,
		m.RecordID, m.Mailbox, m.MailboxName, m.CallerNumber, m.CallerName,
		m.ReceivedAt.Unix(), m.DurationSec,
		nullString(m.AudioPath), nullInt(m.AudioBytes), nullString(m.AudioError),
		m.FetchedAt.Unix(),
	)
	return err
}

// MarkAcked проставляет отметку подтверждения. Вызывается только после того,
// как АТС приняла ack, — иначе acked_at врал бы о состоянии INBOX.
func (r *MessageRepository) MarkAcked(ctx context.Context, mailbox string, recordIDs []string, at time.Time) error {
	if len(recordIDs) == 0 {
		return nil
	}

	args := []any{at.Unix(), mailbox}
	placeholders := make([]string, 0, len(recordIDs))
	for _, id := range recordIDs {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}

	query := fmt.Sprintf(
		"UPDATE messages SET acked_at = ? WHERE mailbox = ? AND record_id IN (%s)",
		strings.Join(placeholders, ", "),
	)

	_, err := r.db.ExecContext(ctx, query, args...)
	return err
}

func (r *MessageRepository) List(ctx context.Context, f Filter) ([]model.Message, int, error) {
	where, args := buildWhere(f)

	var total int
	countQuery := "SELECT count(*) FROM messages" + where
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}

	query := "SELECT" + messageColumns + " FROM messages" + where +
		" ORDER BY received_at DESC, id DESC LIMIT ? OFFSET ?"
	rows, err := r.db.QueryContext(ctx, query, append(args, limit, (page-1)*limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	messages := make([]model.Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, 0, err
		}
		messages = append(messages, *m)
	}

	return messages, total, rows.Err()
}

func (r *MessageRepository) Get(ctx context.Context, id int64) (*model.Message, error) {
	query := "SELECT" + messageColumns + " FROM messages WHERE id = ?"

	m, err := scanMessage(r.db.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// Update меняет статус и/или комментарий. nil-поле означает «не трогать»:
// фронт может прислать только комментарий, не меняя статус, и наоборот.
func (r *MessageRepository) Update(ctx context.Context, id int64, status, comment *string, byID int64, byName string, at time.Time) error {
	const query = `
		UPDATE messages SET
			status          = COALESCE(?, status),
			admin_comment   = COALESCE(?, admin_comment),
			updated_by_id   = ?,
			updated_by_name = ?,
			updated_at      = ?
		WHERE id = ?`

	res, err := r.db.ExecContext(ctx, query,
		nullFromPtr(status), nullFromPtr(comment), byID, byName, at.Unix(), id)
	if err != nil {
		return err
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}

	return nil
}

// Mailboxes собирает справочник ящиков из самих обращений: ходить за ним на АТС
// не нужно, и фильтр в интерфейсе работает, даже когда она недоступна.
func (r *MessageRepository) Mailboxes(ctx context.Context) ([]model.MailboxRef, error) {
	const query = `
		SELECT mailbox, max(mailbox_name), count(*)
		FROM messages
		GROUP BY mailbox
		ORDER BY mailbox`

	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	refs := make([]model.MailboxRef, 0, 19)
	for rows.Next() {
		var ref model.MailboxRef
		var name sql.NullString
		if err := rows.Scan(&ref.Mailbox, &name, &ref.Total); err != nil {
			return nil, err
		}
		ref.Name = name.String
		refs = append(refs, ref)
	}

	return refs, rows.Err()
}

// LastFetchedAt — время последнего удачного забора. Отдельной таблицы состояния
// нет: max(fetched_at) отвечает на этот вопрос бесплатно.
func (r *MessageRepository) LastFetchedAt(ctx context.Context) (time.Time, error) {
	var at sql.NullInt64
	err := r.db.QueryRowContext(ctx, "SELECT max(fetched_at) FROM messages").Scan(&at)
	if err != nil || !at.Valid {
		return time.Time{}, err
	}
	return time.Unix(at.Int64, 0).UTC(), nil
}

// PendingAck — сколько записей сохранено, но ещё висит в INBOX на АТС.
func (r *MessageRepository) PendingAck(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE acked_at IS NULL").Scan(&count)
	return count, err
}

func buildWhere(f Filter) (string, []any) {
	var conds []string
	var args []any

	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.Mailbox != "" {
		conds = append(conds, "mailbox = ?")
		args = append(args, f.Mailbox)
	}
	if !f.From.IsZero() {
		conds = append(conds, "received_at >= ?")
		args = append(args, f.From.Unix())
	}
	if !f.To.IsZero() {
		conds = append(conds, "received_at <= ?")
		args = append(args, f.To.Unix())
	}

	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

type scanner interface {
	Scan(dest ...any) error
}

func scanMessage(s scanner) (*model.Message, error) {
	var m model.Message
	var (
		receivedAt, fetchedAt       int64
		audioPath, audioError       sql.NullString
		adminComment, updatedByName sql.NullString
		audioBytes, updatedByID     sql.NullInt64
		updatedAt, ackedAt          sql.NullInt64
	)

	err := s.Scan(
		&m.ID, &m.RecordID, &m.Mailbox, &m.MailboxName, &m.CallerNumber, &m.CallerName,
		&receivedAt, &m.DurationSec, &audioPath, &audioBytes, &audioError,
		&m.Status, &adminComment, &updatedByID, &updatedByName, &updatedAt,
		&fetchedAt, &ackedAt,
	)
	if err != nil {
		return nil, err
	}

	m.ReceivedAt = time.Unix(receivedAt, 0).UTC()
	m.FetchedAt = time.Unix(fetchedAt, 0).UTC()
	m.AudioPath = audioPath.String
	m.AudioBytes = audioBytes.Int64
	m.AudioError = audioError.String
	m.AdminComment = adminComment.String
	m.UpdatedByID = updatedByID.Int64
	m.UpdatedByName = updatedByName.String
	if updatedAt.Valid {
		m.UpdatedAt = time.Unix(updatedAt.Int64, 0).UTC()
	}
	if ackedAt.Valid {
		m.AckedAt = time.Unix(ackedAt.Int64, 0).UTC()
	}

	return &m, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullFromPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
