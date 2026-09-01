package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"go_voicemail_service_document_flow/internal/model"
	"go_voicemail_service_document_flow/internal/repository"
)

const (
	defaultLimit = 20
	maxLimit     = 100
	maxNameLen   = 255
)

type MessageHandler struct {
	repo     *repository.MessageRepository
	audioDir string
}

func NewMessageHandler(repo *repository.MessageRepository, audioDir string) *MessageHandler {
	return &MessageHandler{repo: repo, audioDir: audioDir}
}

type messageResponse struct {
	ID           int64    `json:"id"`
	RecordID     string   `json:"recordId"`
	Mailbox      string   `json:"mailbox"`
	MailboxName  string   `json:"mailboxName"`
	CallerNumber string   `json:"callerNumber"`
	CallerName   string   `json:"callerName"`
	ReceivedAt   string   `json:"receivedAt"`
	DurationSec  int      `json:"durationSec"`
	HasAudio     bool     `json:"hasAudio"`
	AudioError   string   `json:"audioError,omitempty"`
	Status       string   `json:"status"`
	AdminComment *string  `json:"adminComment"`
	UpdatedBy    *userRef `json:"updatedBy"`
	UpdatedAt    *string  `json:"updatedAt"`
}

type userRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func present(m *model.Message) messageResponse {
	out := messageResponse{
		ID:           m.ID,
		RecordID:     m.RecordID,
		Mailbox:      m.Mailbox,
		MailboxName:  m.MailboxName,
		CallerNumber: m.CallerNumber,
		CallerName:   m.CallerName,
		ReceivedAt:   m.ReceivedAt.Format(time.RFC3339),
		DurationSec:  m.DurationSec,
		HasAudio:     m.HasAudio(),
		AudioError:   m.AudioError,
		Status:       m.Status,
	}

	if m.AdminComment != "" {
		out.AdminComment = &m.AdminComment
	}
	if m.UpdatedByID != 0 {
		out.UpdatedBy = &userRef{ID: m.UpdatedByID, Name: m.UpdatedByName}
	}
	if !m.UpdatedAt.IsZero() {
		formatted := m.UpdatedAt.Format(time.RFC3339)
		out.UpdatedAt = &formatted
	}

	return out
}

func (h *MessageHandler) List() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, err := parseFilter(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		messages, total, err := h.repo.List(r.Context(), filter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Не удалось получить список обращений")
			return
		}

		items := make([]messageResponse, 0, len(messages))
		for i := range messages {
			items = append(items, present(&messages[i]))
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"items":      items,
			"pagination": newPagination(filter.Page, filter.Limit, total),
		})
	}
}

// Audio отдаёт mp3 через http.ServeFile: он сам проставляет audio/mpeg и, что
// важнее, умеет Range — без этого плеер в браузере не сможет перематывать.
func (h *MessageHandler) Audio() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		message, ok := h.load(w, r)
		if !ok {
			return
		}

		if !message.HasAudio() {
			writeError(w, http.StatusNotFound, "У обращения нет записи разговора")
			return
		}

		path := filepath.Join(h.audioDir, filepath.Clean("/"+message.AudioPath))
		if _, err := os.Stat(path); err != nil {
			writeError(w, http.StatusNotFound, "Файл записи не найден")
			return
		}

		w.Header().Set("Cache-Control", "private, max-age=3600")
		http.ServeFile(w, r, path)
	}
}

type patchRequest struct {
	Status        *string `json:"status"`
	AdminComment  *string `json:"adminComment"`
	UpdatedByName *string `json:"updatedByName"`
}

// Patch меняет статус и/или комментарий.
//
// Личность берётся из X-User-Id, который проставляет шлюз, разобрав JWT.
// Имя из тела — только для показа: подделать его можно, но id приезжает по
// другому, подписанному каналу, и по нему всегда видно, кто это был на самом деле.
func (h *MessageHandler) Patch() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		message, ok := h.load(w, r)
		if !ok {
			return
		}

		userID, err := strconv.ParseInt(r.Header.Get("X-User-Id"), 10, 64)
		if err != nil || userID <= 0 {
			// Заголовкам доверяем только потому, что сеть закрыта. Если их нет,
			// запрос пришёл не оттуда, откуда мы думаем, — не «аноним», а отказ.
			writeError(w, http.StatusBadRequest, "Не передан идентификатор пользователя")
			return
		}

		var body patchRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "Некорректное тело запроса")
			return
		}

		if body.Status == nil && body.AdminComment == nil {
			writeError(w, http.StatusBadRequest, "Нечего менять: нужен status или adminComment")
			return
		}

		if body.Status != nil && !model.ValidStatus(*body.Status) {
			writeError(w, http.StatusBadRequest, "Недопустимый статус")
			return
		}

		name := strings.TrimSpace(r.Header.Get("X-User-Name"))
		if body.UpdatedByName != nil {
			if trimmed := strings.TrimSpace(*body.UpdatedByName); trimmed != "" {
				name = trimmed
			}
		}
		if len(name) > maxNameLen {
			name = name[:maxNameLen]
		}

		err = h.repo.Update(r.Context(), message.ID, body.Status, body.AdminComment, userID, name, time.Now().UTC())
		if errors.Is(err, repository.ErrNotFound) {
			writeError(w, http.StatusNotFound, "Обращение не найдено")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Не удалось сохранить изменения")
			return
		}

		updated, err := h.repo.Get(r.Context(), message.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Не удалось перечитать обращение")
			return
		}

		writeJSON(w, http.StatusOK, present(updated))
	}
}

func (h *MessageHandler) Mailboxes() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		refs, err := h.repo.Mailboxes(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Не удалось получить список ящиков")
			return
		}

		items := make([]map[string]any, 0, len(refs))
		for _, ref := range refs {
			items = append(items, map[string]any{
				"mailbox": ref.Mailbox,
				"name":    ref.Name,
				"total":   ref.Total,
			})
		}

		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func (h *MessageHandler) Health() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lastSync, err := h.repo.LastFetchedAt(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "База недоступна")
			return
		}

		pending, err := h.repo.PendingAck(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "База недоступна")
			return
		}

		payload := map[string]any{
			"status":     "ok",
			"service":    "voicemail",
			"pendingAck": pending,
			"lastSyncAt": nil,
		}
		if !lastSync.IsZero() {
			payload["lastSyncAt"] = lastSync.Format(time.RFC3339)
		}

		writeJSON(w, http.StatusOK, payload)
	}
}

func (h *MessageHandler) load(w http.ResponseWriter, r *http.Request) (*model.Message, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "Некорректный идентификатор обращения")
		return nil, false
	}

	message, err := h.repo.Get(r.Context(), id)
	if errors.Is(err, repository.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Обращение не найдено")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не удалось получить обращение")
		return nil, false
	}

	return message, true
}

func parseFilter(r *http.Request) (repository.Filter, error) {
	query := r.URL.Query()

	filter := repository.Filter{
		Page:    intParam(query.Get("page"), 1),
		Limit:   intParam(query.Get("limit"), defaultLimit),
		Status:  query.Get("status"),
		Mailbox: query.Get("mailbox"),
	}

	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.Limit < 1 || filter.Limit > maxLimit {
		filter.Limit = defaultLimit
	}
	if filter.Status != "" && !model.ValidStatus(filter.Status) {
		return filter, errors.New("Недопустимый статус в фильтре")
	}

	var err error
	if raw := query.Get("dateFrom"); raw != "" {
		if filter.From, err = parseDate(raw, false); err != nil {
			return filter, errors.New("Некорректная дата в dateFrom")
		}
	}
	if raw := query.Get("dateTo"); raw != "" {
		if filter.To, err = parseDate(raw, true); err != nil {
			return filter, errors.New("Некорректная дата в dateTo")
		}
	}

	return filter, nil
}

// parseDate принимает и RFC3339, и просто дату из календаря на фронте.
// Для верхней границы день расширяется до конца суток, иначе "по 24 августа"
// отсекало бы всё, что пришло после полуночи.
func parseDate(raw string, endOfDay bool) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC(), nil
	}

	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, err
	}
	if endOfDay {
		parsed = parsed.Add(24*time.Hour - time.Second)
	}

	return parsed.UTC(), nil
}

func intParam(raw string, fallback int) int {
	if value, err := strconv.Atoi(raw); err == nil {
		return value
	}
	return fallback
}
