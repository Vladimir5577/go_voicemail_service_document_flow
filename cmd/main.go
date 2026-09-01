package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"go_voicemail_service_document_flow/internal/app"
	"go_voicemail_service_document_flow/internal/config"
	"go_voicemail_service_document_flow/internal/logger"
)

func main() {
	// -no-ack нужен для первого прогона: ack необратим, запись уходит из INBOX
	// вместе с номером звонившего. Сначала смотрим, что легло в базу.
	syncOnce := flag.Bool("sync", false, "забрать обращения с АТС и выйти")
	noAck := flag.Bool("no-ack", false, "не подтверждать записи на АТС")
	mailbox := flag.String("mailbox", "", "забрать только один ящик, например 090")
	flag.Parse()

	cfg := config.Load()
	logger.Setup(cfg.Env)

	db, err := config.OpenDB(cfg.DBPath)
	if err != nil {
		slog.Error("Не удалось открыть базу", "path", cfg.DBPath, "error", err)
		os.Exit(1)
	}
	defer db.Close()

	application, err := app.NewApp(cfg, db)
	if err != nil {
		slog.Error("Не удалось инициализировать приложение", "error", err)
		os.Exit(1)
	}

	if *syncOnce {
		slog.Info("Ручной забор обращений", "mailbox", *mailbox, "ack", !*noAck)
		if err := application.Sync(context.Background(), *mailbox, !*noAck); err != nil {
			slog.Error("Забор обращений не удался", "error", err)
			os.Exit(1)
		}
		return
	}

	slog.Info("Микросервис голосовой почты запускается", "port", cfg.Port)
	if err := application.Run(); err != nil {
		slog.Error("Ошибка старта HTTP-сервера", "error", err)
		os.Exit(1)
	}
}
