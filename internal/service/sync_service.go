package service

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"go_voicemail_service_document_flow/internal/model"
	"go_voicemail_service_document_flow/internal/repository"
	"go_voicemail_service_document_flow/internal/vmapi"
)

// maxDrainIterations — страховка от бесконечного цикла дренажа, если vmapi
// почему-то перестанет убирать записи из INBOX после ack. При limit=20 это
// потолок в 1000 обращений на ящик за прогон.
const maxDrainIterations = 50

// ackOnlyMailbox — ящик «Нерабочее время». Снежана сказала его удалить,
// поэтому мы его себе не забираем: обращения только подтверждаем на АТС, чтобы
// INBOX не рос и лампа на телефоне гасла, а в базу они не попадают.
//
// ponytail: один ящик хардкодом; список в конфиге — если появится второй.
const ackOnlyMailbox = "090"

type SyncService struct {
	client   *vmapi.Client
	repo     *repository.MessageRepository
	audioDir string
	limit    int
}

func NewSyncService(client *vmapi.Client, repo *repository.MessageRepository, audioDir string, limit int) *SyncService {
	return &SyncService{client: client, repo: repo, audioDir: audioDir, limit: limit}
}

type Stats struct {
	Saved   int
	Acked   int
	Skipped int // сохранены, но без звука — подтверждать нельзя
	Failed  int
}

// Run забирает обращения со всех ящиков, где есть новые.
//
// onlyMailbox ограничивает прогон одним ящиком (для отладки), ack=false
// сохраняет записи, не подтверждая их на АТС, — так делается первый прогон,
// пока портал ещё читает живой INBOX.
func (s *SyncService) Run(ctx context.Context, onlyMailbox string, ack bool) (Stats, error) {
	var total Stats

	mailboxes, err := s.client.Mailboxes(ctx)
	if err != nil {
		return total, err
	}

	for _, mb := range mailboxes {
		if onlyMailbox != "" && mb.Mailbox != onlyMailbox {
			continue
		}
		// Счётчики отдаются одним дешёвым запросом — незачем дёргать
		// девятнадцать ящиков, когда новое есть в двух.
		if mb.New == 0 {
			continue
		}

		stats, err := s.drainMailbox(ctx, mb, ack)
		total.Saved += stats.Saved
		total.Acked += stats.Acked
		total.Skipped += stats.Skipped
		total.Failed += stats.Failed

		if err != nil {
			// Упавший ящик не должен останавливать остальные: то, что не
			// подтверждено, приедет следующей ночью.
			slog.Error("Ящик обработан не полностью",
				"mailbox", mb.Mailbox, "error", err)
		}
	}

	slog.Info("Забор обращений завершён",
		"saved", total.Saved, "acked", total.Acked,
		"skipped", total.Skipped, "failed", total.Failed)

	return total, nil
}

func (s *SyncService) drainMailbox(ctx context.Context, mb vmapi.Mailbox, ack bool) (Stats, error) {
	var stats Stats

	// Записи, которые не удалось подтвердить (например, у них не скачался звук),
	// остаются в INBOX и приезжают в каждой следующей пачке этого же прогона.
	// Без этой отметки мы качали бы их заново на каждой итерации.
	seen := make(map[string]bool)

	for iteration := range maxDrainIterations {
		page, err := s.client.Messages(ctx, mb.Mailbox, s.limit)
		if err != nil {
			return stats, err
		}
		if len(page.Messages) == 0 {
			return stats, nil
		}

		// Подтверждаем только то, что действительно легло в базу вместе с mp3.
		// Отправить сюда весь список из ответа — единственный способ потерять
		// обращение навсегда: оно уйдёт из INBOX, а номер звонившего нигде не
		// сохранится.
		var complete []string

		for _, msg := range page.Messages {
			if seen[msg.ID] {
				continue
			}
			seen[msg.ID] = true

			// «Нерабочее время» не сохраняем — сразу в список на подтверждение.
			if mb.Mailbox == ackOnlyMailbox {
				complete = append(complete, msg.ID)
				continue
			}

			saved, err := s.saveOne(ctx, mb, msg)
			switch {
			case err != nil:
				stats.Failed++
				slog.Error("Обращение не сохранено",
					"mailbox", mb.Mailbox, "record_id", msg.ID, "error", err)
			case saved:
				stats.Saved++
				complete = append(complete, msg.ID)
			default:
				stats.Saved++
				stats.Skipped++
			}
		}

		if ack && len(complete) > 0 {
			acked, err := s.ackBatch(ctx, mb.Mailbox, complete)
			if err != nil {
				return stats, err
			}
			stats.Acked += acked
		}

		if !page.Truncated {
			return stats, nil
		}
		if !ack {
			// Без подтверждения следующий запрос вернёт ту же пачку — дренаж
			// превратился бы в вечный цикл.
			slog.Warn("Остались необработанные обращения: без ack дренаж не продолжается",
				"mailbox", mb.Mailbox)
			return stats, nil
		}
		if len(complete) == 0 {
			// Ничего не подтверждено — INBOX не сдвинулся, и следующая пачка
			// будет той же самой. Остаток разберём в следующий прогон.
			slog.Warn("Пачка не подтверждена целиком, дренаж остановлен",
				"mailbox", mb.Mailbox)
			return stats, nil
		}
		if iteration == maxDrainIterations-1 {
			slog.Warn("Достигнут потолок итераций дренажа, остаток заберётся в следующий прогон",
				"mailbox", mb.Mailbox)
		}
	}

	return stats, nil
}

// saveOne проводит одно обращение через весь порядок: файл → строка → отметка
// на подтверждение. Возвращает true, только если запись сохранена целиком, со
// звуком, — лишь такую можно подтверждать на АТС.
func (s *SyncService) saveOne(ctx context.Context, mb vmapi.Mailbox, msg vmapi.Message) (bool, error) {
	now := time.Now().UTC()
	received := time.Unix(msg.ReceivedEpoch, 0).UTC()

	record := &model.Message{
		RecordID:     msg.ID,
		Mailbox:      mb.Mailbox,
		MailboxName:  mb.Name,
		CallerNumber: msg.CallerNumber,
		CallerName:   msg.CallerName,
		ReceivedAt:   received,
		DurationSec:  msg.DurationSec,
		FetchedAt:    now,
	}

	relPath, err := audioRelPath(mb.Mailbox, msg.ID, received)
	if err != nil {
		return false, err
	}

	// Звук качаем первым: файл нельзя откатить транзакцией, поэтому коммит
	// строки должен означать «всё на месте». В обратном порядке падение между
	// коммитом и записью файла оставило бы строку с путём на несуществующий
	// файл, а апсерт с условием audio_path IS NULL её бы уже не починил.
	written, audioErr := s.fetchAudio(ctx, mb.Mailbox, msg.ID, relPath)
	switch {
	case audioErr != nil:
		// Метаданные сохраняем всё равно: номер звонившего и время живут
		// только пока запись в INBOX, восстановить их будет неоткуда.
		// Без ack запись приедет снова, и апсерт допишет ей звук.
		record.AudioError = audioErr.Error()
		slog.Warn("Запись сохранена без звука",
			"mailbox", mb.Mailbox, "record_id", msg.ID, "error", audioErr)
	default:
		record.AudioPath = relPath
		record.AudioBytes = written
	}

	if err := s.repo.Upsert(ctx, record); err != nil {
		return false, err
	}

	return audioErr == nil, nil
}

func (s *SyncService) fetchAudio(ctx context.Context, mailbox, recordID, relPath string) (int64, error) {
	body, err := s.client.Audio(ctx, mailbox, recordID)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	return writeFileAtomic(filepath.Join(s.audioDir, relPath), body)
}

func (s *SyncService) ackBatch(ctx context.Context, mailbox string, recordIDs []string) (int, error) {
	result, err := s.client.Ack(ctx, mailbox, recordIDs)
	if err != nil {
		return 0, err
	}

	// not_found — не ошибка: сотрудник мог прослушать запись с телефона через
	// *97, и Asterisk убрал её из новых сам. Для нас она обработана.
	confirmed := append(append([]string{}, result.Moved...), result.NotFound...)
	if err := s.repo.MarkAcked(ctx, mailbox, confirmed, time.Now().UTC()); err != nil {
		return 0, err
	}

	return len(confirmed), nil
}
