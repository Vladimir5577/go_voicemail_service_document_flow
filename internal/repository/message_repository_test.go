package repository

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go_voicemail_service_document_flow/internal/model"

	_ "modernc.org/sqlite"
)

func newTestRepo(t *testing.T) *MessageRepository {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	schema, err := os.ReadFile("../../migrations/00001_init_voicemail_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Берём только секцию Up — goose-аннотации SQLite не понимает.
	up, _, _ := strings.Cut(string(schema), "-- +goose Down")
	if _, err := db.Exec(strings.TrimPrefix(up, "-- +goose Up")); err != nil {
		t.Fatal(err)
	}

	return NewMessageRepository(db)
}

func sample(recordID string, audioPath string) *model.Message {
	return &model.Message{
		RecordID:     recordID,
		Mailbox:      "090",
		MailboxName:  "Нерабочее время",
		CallerNumber: "+79495352139",
		ReceivedAt:   time.Unix(1787591171, 0).UTC(),
		DurationSec:  9,
		AudioPath:    audioPath,
		AudioBytes:   39501,
		FetchedAt:    time.Unix(1787600000, 0).UTC(),
	}
}

// Пока запись не подтверждена через ack, vmapi отдаёт её каждую ночь заново.
// Повтор не должен ни плодить строки, ни затирать работу администратора.
func TestUpsertIsIdempotentAndKeepsAdminFields(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.Upsert(ctx, sample("1787591171-00000002", "2026/08/090-1.mp3")); err != nil {
		t.Fatal(err)
	}

	messages, total, err := repo.List(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("после первой вставки строк %d, ожидалась 1", total)
	}

	status := model.StatusSpam
	comment := "перезвонил, реклама"
	if err := repo.Update(ctx, messages[0].ID, &status, &comment, 5, "Петров В.А.", time.Now()); err != nil {
		t.Fatal(err)
	}

	// Та же запись приезжает снова.
	if err := repo.Upsert(ctx, sample("1787591171-00000002", "2026/08/090-1.mp3")); err != nil {
		t.Fatal(err)
	}

	messages, total, err = repo.List(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("после повтора строк %d, ожидалась 1", total)
	}
	if messages[0].Status != model.StatusSpam {
		t.Errorf("статус = %q, повторный забор его затёр", messages[0].Status)
	}
	if messages[0].AdminComment != comment {
		t.Errorf("комментарий = %q, повторный забор его затёр", messages[0].AdminComment)
	}
}

// Запись без звука сохраняется сразу (номер звонившего восстановить неоткуда),
// а mp3 подтягивается следующим прогоном.
func TestUpsertFillsMissingAudio(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	broken := sample("1787591171-00000003", "")
	broken.AudioError = "ffmpeg failed"
	if err := repo.Upsert(ctx, broken); err != nil {
		t.Fatal(err)
	}

	fixed := sample("1787591171-00000003", "2026/08/090-3.mp3")
	if err := repo.Upsert(ctx, fixed); err != nil {
		t.Fatal(err)
	}

	messages, _, err := repo.List(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("строк %d, ожидалась 1", len(messages))
	}
	if messages[0].AudioPath != "2026/08/090-3.mp3" {
		t.Errorf("audio_path = %q, звук не дописался", messages[0].AudioPath)
	}
}

func TestMarkAckedOnlyListedRecords(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.Upsert(ctx, sample("1787591171-00000002", "2026/08/090-1.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, sample("1787591171-00000004", "2026/08/090-4.mp3")); err != nil {
		t.Fatal(err)
	}

	if err := repo.MarkAcked(ctx, "090", []string{"1787591171-00000002"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	pending, err := repo.PendingAck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Errorf("неподтверждённых %d, ожидалась 1", pending)
	}
}
