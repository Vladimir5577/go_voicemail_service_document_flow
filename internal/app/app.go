package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"go_voicemail_service_document_flow/internal/config"
	"go_voicemail_service_document_flow/internal/handler"
	"go_voicemail_service_document_flow/internal/repository"
	"go_voicemail_service_document_flow/internal/service"
	"go_voicemail_service_document_flow/internal/vmapi"
)

type App struct {
	router *chi.Mux
	cfg    *config.Config
	sync   *service.SyncService
}

func NewApp(cfg *config.Config, db *sql.DB) (*App, error) {
	if err := os.MkdirAll(cfg.AudioDir, 0o755); err != nil {
		return nil, fmt.Errorf("не удалось создать каталог для записей: %w", err)
	}

	repo := repository.NewMessageRepository(db)
	client := vmapi.New(cfg.VmapiURL, cfg.VmapiToken, cfg.VmapiTimeout)

	return &App{
		router: setupRouter(handler.NewMessageHandler(repo, cfg.AudioDir)),
		cfg:    cfg,
		sync:   service.NewSyncService(client, repo, cfg.AudioDir, cfg.VmapiLimit),
	}, nil
}

// Sync — точка входа для консольной команды ./main -sync.
func (a *App) Sync(ctx context.Context, mailbox string, ack bool) error {
	_, err := a.sync.Run(ctx, mailbox, ack)
	return err
}

func (a *App) Run() error {
	appCtx, stopBackground := context.WithCancel(context.Background())
	var backgroundWG sync.WaitGroup

	if len(a.cfg.PollAt) > 0 {
		backgroundWG.Add(1)
		go func() {
			defer backgroundWG.Done()
			a.runSchedule(appCtx)
		}()
	}

	srv := &http.Server{
		Addr:         ":" + a.cfg.Port,
		Handler:      a.router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second, // отдача mp3 идёт через этот же сервер
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Ошибка старта HTTP-сервера", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("Получен сигнал завершения, начинаем graceful shutdown...")
	stopBackground()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("ошибка при остановке сервера: %w", err)
	}

	backgroundDone := make(chan struct{})
	go func() {
		backgroundWG.Wait()
		close(backgroundDone)
	}()

	select {
	case <-backgroundDone:
	case <-ctx.Done():
		slog.Warn("Ночной забор не успел завершиться до таймаута")
	}

	slog.Info("HTTP-сервер успешно остановлен")
	return nil
}

// runSchedule гоняет забор по расписанию из POLL_AT.
//
// Первый прогон — сразу на старте: контейнер, перезапущенный в 03:29 и
// поднявшийся в 03:31, иначе пропустил бы окно, и обращения ждали бы сутки.
// Повторный прогон безвреден — забор идемпотентен.
func (a *App) runSchedule(ctx context.Context) {
	a.runOnce(ctx)

	for {
		next, err := service.NextRun(time.Now(), a.cfg.PollAt)
		if err != nil {
			slog.Error("Расписание не разобрано, ночной забор выключен", "error", err)
			return
		}

		slog.Info("Следующий забор обращений", "at", next.Format(time.RFC3339))

		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			a.runOnce(ctx)
		}
	}
}

func (a *App) runOnce(ctx context.Context) {
	if _, err := a.sync.Run(ctx, "", true); err != nil {
		slog.Error("Забор обращений не удался", "error", err)
	}
}
