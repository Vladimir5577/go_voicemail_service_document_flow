package app

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"

	"go_voicemail_service_document_flow/internal/handler"
)

// Публичный префикс совпадает с тем, по которому фронт ходил в прокси раньше:
// шлюз проксирует сюда весь путь целиком, ничего не срезая.
const apiPrefix = "/spa/api/external-api/voicemail"

// setupRouter. Своей авторизации у сервиса нет: JWT и роль проверяет шлюз,
// а сюда попадают только запросы из закрытой сети voicemail-net. Личность
// пользователя приезжает заголовками X-User-Id и X-User-Name.
func setupRouter(h *handler.MessageHandler) *chi.Mux {
	r := chi.NewRouter()

	r.Use(chiMiddleware.RequestID)
	r.Use(chiMiddleware.Logger)
	r.Use(chiMiddleware.Recoverer)
	r.Use(chiMiddleware.Timeout(30 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status": "ok", "service": "voicemail"}`))
	})

	r.Route(apiPrefix, func(r chi.Router) {
		r.Get("/health", h.Health())
		r.Get("/mailboxes", h.Mailboxes())
		r.Get("/messages", h.List())
		r.Get("/messages/{id}/audio", h.Audio())
		r.Patch("/messages/{id}", h.Patch())
	})

	return r
}
