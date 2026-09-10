package config

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	_ "modernc.org/sqlite"
)

type Config struct {
	Env  string
	Port string

	// DBPath — файл SQLite, AudioDir — корень для mp3.
	DBPath   string
	AudioDir string

	// vmapi на самой АТС.
	VmapiURL     string
	VmapiToken   string
	VmapiTimeout time.Duration
	VmapiLimit   int

	// PollAt — времена ночного забора в формате "03:30". Пустой список выключает
	// расписание целиком: сервис поднимется и будет только отдавать данные.
	// Включаем последним, когда фронт переедет на новые ручки: первый же ack
	// начнёт вычищать INBOX, и старая страница портала опустеет.
	PollAt []string
}

func Load() *Config {
	if err := godotenv.Load(); err != nil {
		slog.Warn(".env не найден, используются системные переменные окружения")
	}

	c := &Config{
		Env:      getEnv("ENV", "local"),
		Port:     getEnv("SERVER_PORT", "8089"),
		DBPath:   getEnv("DB_PATH", "./data/voicemail.db"),
		AudioDir: getEnv("AUDIO_DIR", "./audio"),

		VmapiURL:     getEnv("VMAPI_URL", ""),
		VmapiToken:   getEnv("VMAPI_TOKEN", ""),
		VmapiTimeout: time.Duration(getEnvAsInt("VMAPI_TIMEOUT_SECONDS", 60)) * time.Second,
		VmapiLimit:   getEnvAsInt("VMAPI_LIMIT", 20),

		PollAt: splitList(getEnv("POLL_AT", "")),
	}

	if c.VmapiURL == "" {
		slog.Warn("VMAPI_URL не задан — забор обращений работать не будет")
	}
	if c.VmapiToken == "" {
		slog.Warn("VMAPI_TOKEN не задан — АТС отклонит запросы")
	}
	if len(c.PollAt) == 0 {
		slog.Warn("POLL_AT не задан — ночной забор выключен, работает только ручной запуск ./main -sync")
	}

	return c
}

// OpenDB открывает SQLite и приводит его в рабочее состояние.
//
// Четыре прагмы, каждая по своей причине:
//   - WAL: читатели не блокируют писателя, и файл переживает падение процесса;
//   - busy_timeout: консольная команда ./main -sync — отдельный процесс, и он
//     может писать одновременно с сервисом; MaxOpenConns тут не помогает;
//   - synchronous(FULL): коммит фиксируется на диск до возврата. Обычный для WAL
//     NORMAL допускает потерю последних коммитов при отключении питания, а мы
//     сразу после коммита подтверждаем запись на АТС — она уйдёт из INBOX, и
//     потерянную строку взять будет неоткуда;
//   - foreign_keys: в SQLite внешние ключи по умолчанию выключены, и REFERENCES
//     остаётся комментарием. Без неё ON DELETE CASCADE у message_plays не
//     сработает, и удалённое обращение оставит за собой журнал прослушиваний.
func OpenDB(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("не удалось создать каталог для БД: %w", err)
		}
	}

	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Один писатель на процесс: database/sql выстраивает всех в очередь сам,
	// и SQLITE_BUSY внутри процесса не возникает в принципе. Чтения при нашей
	// нагрузке (несколько админов) от сериализации не страдают.
	// ponytail: один коннект на всё; разделить на read/write пулы, если упрёмся.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvAsInt(key string, fallback int) int {
	if value, err := strconv.Atoi(getEnv(key, "")); err == nil {
		return value
	}
	return fallback
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
