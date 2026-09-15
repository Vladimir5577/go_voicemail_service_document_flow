package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
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
	PlayCount    int      `json:"playCount"`
	Status       string   `json:"status"`
	StatusBy     *userRef `json:"statusBy"`
	StatusAt     *string  `json:"statusAt"`
	AdminComment *string  `json:"adminComment"`
	CommentBy    *userRef `json:"commentBy"`
	CommentAt    *string  `json:"commentAt"`
}

type userRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type playResponse struct {
	User     userRef `json:"user"`
	PlayedAt string  `json:"playedAt"`
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
		PlayCount:    m.PlayCount,
		Status:       m.Status,
	}

	if m.AdminComment != "" {
		out.AdminComment = &m.AdminComment
	}
	if m.StatusByID != 0 {
		out.StatusBy = &userRef{ID: m.StatusByID, Name: m.StatusByName}
	}
	if !m.StatusAt.IsZero() {
		formatted := m.StatusAt.Format(time.RFC3339)
		out.StatusAt = &formatted
	}
	if m.CommentByID != 0 {
		out.CommentBy = &userRef{ID: m.CommentByID, Name: m.CommentByName}
	}
	if !m.CommentAt.IsZero() {
		formatted := m.CommentAt.Format(time.RFC3339)
		out.CommentAt = &formatted
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

		userID, ok := h.requestUser(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "Не передан идентификатор пользователя")
			return
		}

		// Range-запросы не считаем: одно прослушивание в потоковом плеере — это
		// пачка догрузок хвоста, и счётчик показывал бы их, а не людей. Открытие
		// записи — это всегда первый запрос, без Range.
		if r.Header.Get("Range") == "" {
			// Журнал не должен мешать работе: не записалась отметка — человек всё
			// равно слушает обращение. Аудио тут продукт, учёт — бухгалтерия.
			if err := h.repo.AddPlay(r.Context(), message.ID, userID, time.Now().UTC()); err != nil {
				slog.Error("Не удалось записать отметку о прослушивании",
					"message_id", message.ID, "user_id", userID, "error", err)
			}
		}

		// no-store, а не max-age: с кэшем повторное открытие в пределах часа не
		// дошло бы до сервиса, и журнал молча пропускал бы половину прослушиваний.
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, path)
	}
}

// Plays отдаёт журнал прослушиваний одного обращения: кто открывал запись и когда.
// total — это и есть «сколько раз».
func (h *MessageHandler) Plays() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		message, ok := h.load(w, r)
		if !ok {
			return
		}

		plays, err := h.repo.Plays(r.Context(), message.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Не удалось получить журнал прослушиваний")
			return
		}

		items := make([]playResponse, 0, len(plays))
		for _, play := range plays {
			items = append(items, playResponse{
				User:     userRef{ID: play.UserID, Name: play.UserName},
				PlayedAt: play.PlayedAt.Format(time.RFC3339),
			})
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"items": items,
			"total": len(items),
		})
	}
}

// requestUser достаёт личность из заголовков и заводит человека в справочнике,
// если тот пришёл впервые.
//
// Заголовки тут двух разных пород, и доверие к ним разное.
//
// X-User-Id и X-User-Name ставит шлюз, разобрав JWT, а одноимённые от клиента
// затирает. Поэтому отсутствие X-User-Id означает, что запрос пришёл мимо
// шлюза, — это отказ, а не «аноним».
//
// X-User-Display-Name, наоборот, ставит сам фронт, и шлюз его не трогает: ФИО
// в токене нет, знает его только клиент. Подделать такое имя можно — но это
// косметика, а кто человек на самом деле, видно по user_id, который приехал
// подписанным каналом. Подпись под статусом и в журнале строится по id.
//
// Промах справочника не должен ломать запрос: не завёлся пользователь — человек
// всё равно слушает запись, а в журнале останется id без имени.
func (h *MessageHandler) requestUser(r *http.Request) (int64, bool) {
	userID, err := strconv.ParseInt(r.Header.Get("X-User-Id"), 10, 64)
	if err != nil || userID <= 0 {
		return 0, false
	}

	// У PATCH имя приезжает в теле, но у GET за записью тела нет — поэтому
	// заголовком. Процентами, потому что в HTTP-заголовок кириллицу не положить:
	// fetch падает с TypeError на первой же «Житнушкиной».
	//
	// Логин остаётся запасным вариантом: запрос мимо фронта — курлом, из
	// Postman — заголовка не принесёт, и человек всё равно должен завестись.
	name := displayName(r)
	if name == "" {
		name = strings.TrimSpace(r.Header.Get("X-User-Name"))
		if len(name) > maxNameLen {
			name = name[:maxNameLen]
		}
	}

	if err := h.repo.EnsureUser(r.Context(), userID, name, time.Now().UTC()); err != nil {
		slog.Error("Не удалось завести пользователя в справочнике",
			"user_id", userID, "error", err)
	}

	return userID, true
}

// displayName достаёт ФИО из заголовка фронта. Пустая строка — заголовка нет
// или он битый: имя косметика, из-за него запрос заваливать нечего.
func displayName(r *http.Request) string {
	raw := r.Header.Get("X-User-Display-Name")
	if raw == "" {
		return ""
	}

	decoded, err := url.PathUnescape(raw)
	if err != nil {
		slog.Warn("Не удалось раскодировать X-User-Display-Name", "value", raw)
		return ""
	}

	decoded = strings.TrimSpace(decoded)
	if len(decoded) > maxNameLen {
		decoded = decoded[:maxNameLen]
	}

	return decoded
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

		userID, ok := h.requestUser(r)
		if !ok {
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

		// В JWT лежит логин, а ФИО знает только фронт. Прислал — записываем его
		// в справочник: тогда «v.petrov» превращается в человека разом везде,
		// и в списке обращений, и в журналах прослушиваний за прошлые дни.
		if body.UpdatedByName != nil {
			if trimmed := strings.TrimSpace(*body.UpdatedByName); trimmed != "" {
				if len(trimmed) > maxNameLen {
					trimmed = trimmed[:maxNameLen]
				}
				if err := h.repo.SetUserName(r.Context(), userID, trimmed); err != nil {
					slog.Error("Не удалось обновить имя пользователя",
						"user_id", userID, "error", err)
				}
			}
		}

		err := h.repo.Update(r.Context(), message.ID, body.Status, body.AdminComment, userID, time.Now().UTC())
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
