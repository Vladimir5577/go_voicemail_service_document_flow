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

// messageColumns общий для List и Get, поэтому счётчик прослушиваний
// приезжает везде, где обращение отдаётся целиком.
//
// Подзапросом, а не через LEFT JOIN + GROUP BY: он считается уже после LIMIT,
// то есть двадцать раз на страницу и каждый — попаданием в
// idx_message_plays_message. Группировка перемолола бы всю выборку целиком.
const messageColumns = `
	id, record_id, mailbox, mailbox_name, caller_number, caller_name,
	received_at, duration_sec, audio_path, audio_bytes, audio_error,
	status, status_by_id,
	(SELECT name FROM users WHERE id = messages.status_by_id),
	status_at,
	admin_comment, comment_by_id,
	(SELECT name FROM users WHERE id = messages.comment_by_id),
	comment_at,
	fetched_at, acked_at,
	(SELECT count(*) FROM message_plays WHERE message_id = messages.id)`

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
//
// SET собирается по факту присланного, а не через COALESCE: подпись должна
// доставаться тому, кто менял **это** поле. Пришёл один комментарий — колонки
// статуса не трогаются вовсе, и его прежний автор остаётся на месте.
//
// Имя не пишется: оно живёт в users, здесь остаётся только id.
func (r *MessageRepository) Update(ctx context.Context, id int64, status, comment *string, byID int64, at time.Time) error {
	var sets []string
	var args []any

	if status != nil {
		sets = append(sets, "status = ?", "status_by_id = ?", "status_at = ?")
		args = append(args, *status, byID, at.Unix())
	}
	if comment != nil {
		sets = append(sets, "admin_comment = ?", "comment_by_id = ?", "comment_at = ?")
		args = append(args, *comment, byID, at.Unix())
	}
	if len(sets) == 0 {
		// Хендлер такое отсекает раньше, но пустой UPDATE всё равно бессмыслен.
		return nil
	}
	args = append(args, id)

	query := "UPDATE messages SET " + strings.Join(sets, ", ") + " WHERE id = ?"

	res, err := r.db.ExecContext(ctx, query, args...)
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

// EnsureUser заводит пользователя, если его ещё нет.
//
// Сначала чтение, и только при промахе запись: у известного человека — а это
// подавляющее большинство запросов — операция сводится к поиску по первичному
// ключу, без единой записи на диск. Апсертом было бы короче на строку, но он
// писал бы на каждый запрос ради имени, которое всё равно не меняется.
//
// INSERT OR IGNORE, а не голый INSERT: два запроса от нового человека могут
// разойтись между проверкой и вставкой, и второй словил бы ошибку по ключу.
func (r *MessageRepository) EnsureUser(ctx context.Context, id int64, name string, at time.Time) error {
	var exists int
	err := r.db.QueryRowContext(ctx, "SELECT 1 FROM users WHERE id = ?", id).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	_, err = r.db.ExecContext(ctx,
		"INSERT OR IGNORE INTO users (id, name, seen_at) VALUES (?, ?, ?)",
		id, name, at.Unix())
	return err
}

// SetUserName обновляет имя. Вызывается только когда фронт явно прислал ФИО:
// в JWT лежит логин, и первый же PATCH с updatedByName превращает «v.petrov»
// в человеческое имя — для всех записей сразу, и прошлых, и будущих.
func (r *MessageRepository) SetUserName(ctx context.Context, id int64, name string) error {
	_, err := r.db.ExecContext(ctx, "UPDATE users SET name = ? WHERE id = ?", name, id)
	return err
}

// AddPlay отмечает, что запись разговора открыли. Имя не дублируется — оно
// в users, здесь только id.
func (r *MessageRepository) AddPlay(ctx context.Context, messageID, userID int64, at time.Time) error {
	const query = `
		INSERT INTO message_plays (message_id, user_id, played_at)
		VALUES (?, ?, ?)`

	_, err := r.db.ExecContext(ctx, query, messageID, userID, at.Unix())
	return err
}

// Plays отдаёт журнал прослушиваний обращения, свежие сверху.
//
// LEFT JOIN, а не INNER: если пользователя в справочнике почему-то нет,
// прослушивание всё равно должно быть видно — с пустым именем, но с id.
func (r *MessageRepository) Plays(ctx context.Context, messageID int64) ([]model.Play, error) {
	const query = `
		SELECT p.user_id, u.name, p.played_at
		FROM message_plays p
		LEFT JOIN users u ON u.id = p.user_id
		WHERE p.message_id = ?
		ORDER BY p.played_at DESC, p.id DESC`

	rows, err := r.db.QueryContext(ctx, query, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	plays := make([]model.Play, 0)
	for rows.Next() {
		var play model.Play
		var name sql.NullString
		var playedAt int64
		if err := rows.Scan(&play.UserID, &name, &playedAt); err != nil {
			return nil, err
		}
		play.UserName = name.String
		play.PlayedAt = time.Unix(playedAt, 0).UTC()
		plays = append(plays, play)
	}

	return plays, rows.Err()
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
		adminComment                sql.NullString
		statusByName, commentByName sql.NullString
		audioBytes, ackedAt         sql.NullInt64
		statusByID, statusAt        sql.NullInt64
		commentByID, commentAt      sql.NullInt64
	)

	err := s.Scan(
		&m.ID, &m.RecordID, &m.Mailbox, &m.MailboxName, &m.CallerNumber, &m.CallerName,
		&receivedAt, &m.DurationSec, &audioPath, &audioBytes, &audioError,
		&m.Status, &statusByID, &statusByName, &statusAt,
		&adminComment, &commentByID, &commentByName, &commentAt,
		&fetchedAt, &ackedAt, &m.PlayCount,
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
	m.StatusByID = statusByID.Int64
	m.StatusByName = statusByName.String
	m.CommentByID = commentByID.Int64
	m.CommentByName = commentByName.String
	if statusAt.Valid {
		m.StatusAt = time.Unix(statusAt.Int64, 0).UTC()
	}
	if commentAt.Valid {
		m.CommentAt = time.Unix(commentAt.Int64, 0).UTC()
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
