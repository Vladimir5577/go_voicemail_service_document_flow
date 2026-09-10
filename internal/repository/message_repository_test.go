package repository

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
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

	// Все миграции по порядку имён — так же, как их катит goose.
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)

	for _, file := range files {
		schema, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		// Берём только секцию Up — goose-аннотации SQLite не понимает.
		up, _, _ := strings.Cut(string(schema), "-- +goose Down")
		if _, err := db.Exec(strings.TrimPrefix(up, "-- +goose Up")); err != nil {
			t.Fatalf("%s: %v", filepath.Base(file), err)
		}
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
	if err := repo.EnsureUser(ctx, 5, "Петров В.А.", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(ctx, messages[0].ID, &status, &comment, 5, time.Now()); err != nil {
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

// Справочник: первый приход заводит человека, повторные ничего не трогают,
// а явно присланное ФИО заменяет логин разом во всех прошлых записях.
func TestEnsureUserInsertsOnceAndNameFlowsIntoJoins(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	at := time.Unix(1787591200, 0).UTC()
	if err := repo.EnsureUser(ctx, 77, "v.petrov", at); err != nil {
		t.Fatal(err)
	}
	// Второй приход того же человека: имя и seen_at должны остаться прежними.
	if err := repo.EnsureUser(ctx, 77, "кто-то другой", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := repo.Upsert(ctx, sample("1787591171-00000002", "2026/08/090-1.mp3")); err != nil {
		t.Fatal(err)
	}
	messages, _, err := repo.List(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	id := messages[0].ID

	if err := repo.AddPlay(ctx, id, 77, at); err != nil {
		t.Fatal(err)
	}

	plays, err := repo.Plays(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 1 || plays[0].UserName != "v.petrov" {
		t.Fatalf("повторный EnsureUser затёр имя: %+v", plays)
	}

	// Фронт прислал ФИО — оно должно подхватиться и в журнале за прошлое,
	// и в списке обращений.
	if err := repo.SetUserName(ctx, 77, "Петров Владимир Алексеевич"); err != nil {
		t.Fatal(err)
	}
	status := model.StatusDone
	if err := repo.Update(ctx, id, &status, nil, 77, at); err != nil {
		t.Fatal(err)
	}

	plays, err = repo.Plays(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if plays[0].UserName != "Петров Владимир Алексеевич" {
		t.Errorf("в журнале имя = %q, ожидалось ФИО", plays[0].UserName)
	}

	messages, _, err = repo.List(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if messages[0].StatusByName != "Петров Владимир Алексеевич" {
		t.Errorf("в списке status_by_name = %q, join не сработал", messages[0].StatusByName)
	}
}

// Правка комментария не должна переписывать авторство статуса — ровно тот
// случай, из-за которого поля и разделили: Иванов поставил статус, Петров
// через три дня дописал комментарий.
func TestUpdateKeepsStatusAuthorWhenOnlyCommentChanges(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.Upsert(ctx, sample("1787591171-00000002", "2026/08/090-1.mp3")); err != nil {
		t.Fatal(err)
	}
	messages, _, err := repo.List(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	id := messages[0].ID

	ivanov, petrov := int64(11), int64(22)
	at := time.Unix(1787591200, 0).UTC()
	if err := repo.EnsureUser(ctx, ivanov, "Иванов", at); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureUser(ctx, petrov, "Петров", at); err != nil {
		t.Fatal(err)
	}

	status := model.StatusInProgress
	if err := repo.Update(ctx, id, &status, nil, ivanov, at); err != nil {
		t.Fatal(err)
	}

	// Три дня спустя другой человек трогает только комментарий.
	later := at.Add(72 * time.Hour)
	comment := "перезвонил, вопрос решён"
	if err := repo.Update(ctx, id, nil, &comment, petrov, later); err != nil {
		t.Fatal(err)
	}

	m, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if m.StatusByID != ivanov {
		t.Errorf("статус приписан %d, а ставил его Иванов (%d)", m.StatusByID, ivanov)
	}
	if m.StatusByName != "Иванов" {
		t.Errorf("имя автора статуса = %q", m.StatusByName)
	}
	if !m.StatusAt.Equal(at) {
		t.Errorf("время статуса = %v, ожидалось %v", m.StatusAt, at)
	}
	if m.CommentByID != petrov {
		t.Errorf("комментарий приписан %d, а писал его Петров (%d)", m.CommentByID, petrov)
	}
	if !m.CommentAt.Equal(later) {
		t.Errorf("время комментария = %v, ожидалось %v", m.CommentAt, later)
	}
	if m.Status != model.StatusInProgress {
		t.Errorf("статус = %q, правка комментария его сдвинула", m.Status)
	}
}

// Журнал прослушиваний должен отвечать сразу на оба вопроса: сколько раз
// (число строк) и кто именно — имя приезжает join-ом из справочника.
func TestPlaysCountsEveryOpenAndKeepsNames(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.Upsert(ctx, sample("1787591171-00000002", "2026/08/090-1.mp3")); err != nil {
		t.Fatal(err)
	}
	messages, _, err := repo.List(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	id := messages[0].ID

	at := time.Unix(1787591200, 0).UTC()
	if err := repo.EnsureUser(ctx, 77, "Петров Владимир Алексеевич", at); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureUser(ctx, 92, "Иванова Анна Сергеевна", at); err != nil {
		t.Fatal(err)
	}

	if err := repo.AddPlay(ctx, id, 77, at); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddPlay(ctx, id, 92, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Тот же человек второй раз — это отдельная строка, а не апсерт.
	if err := repo.AddPlay(ctx, id, 77, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	plays, err := repo.Plays(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 3 {
		t.Fatalf("прослушиваний %d, ожидалось 3", len(plays))
	}
	if plays[0].UserID != 77 || plays[0].PlayedAt != at.Add(2*time.Minute) {
		t.Errorf("сверху должно быть свежее, получено %+v", plays[0])
	}
	if plays[1].UserName != "Иванова Анна Сергеевна" {
		t.Errorf("имя не сохранилось: %q", plays[1].UserName)
	}

	// Тот же счётчик приезжает числом в списке — фронту хватает его, без журнала.
	listed, _, err := repo.List(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if listed[0].PlayCount != 3 {
		t.Errorf("play_count в списке = %d, ожидалось 3", listed[0].PlayCount)
	}

	// Чужое обращение не должно видеть этот журнал.
	other, err := repo.Plays(ctx, id+1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("у постороннего обращения %d прослушиваний, ожидалось 0", len(other))
	}
}
