package service

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// safeSegment — граница доверия. mailbox и record_id приходят с внешнего
// сервиса и идут прямо в имя файла; без проверки record_id вида
// "../../etc/cron.d/x" запишет файл куда угодно на диске.
var safeSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// audioRelPath строит путь вида "2026/08/090-1787591171-00000002.mp3".
//
// Раскладка по месяцу, а не по ящику: по файловой системе руками не ходят,
// отдел ищется запросом к базе. Деление на 19 ящиков размазало бы файлы так,
// что в половине каталогов лежал бы один файл за месяц.
//
// Имя — пара mailbox+record_id, а не наш id: он известен только после INSERT,
// а файл пишется до него. Эта пара и так уникальна (UNIQUE в схеме), плюс по
// имени видно, чей ящик, если файл нашёлся отдельно от базы.
func audioRelPath(mailbox, recordID string, receivedAt time.Time) (string, error) {
	if !safeSegment.MatchString(mailbox) {
		return "", fmt.Errorf("недопустимый номер ящика: %q", mailbox)
	}
	if !safeSegment.MatchString(recordID) {
		return "", fmt.Errorf("недопустимый идентификатор записи: %q", recordID)
	}

	return filepath.Join(
		receivedAt.Format("2006"),
		receivedAt.Format("01"),
		mailbox+"-"+recordID+".mp3",
	), nil
}

// writeFileAtomic пишет во временный файл рядом с целевым и переименовывает.
//
// rename атомарен в пределах одной ФС, поэтому недокачанного файла под
// итоговым именем не бывает. Sync перед этим обязателен: иначе при отключении
// питания в базе останется строка, указывающая на пустой файл, а запись с АТС
// уже уйдёт из INBOX по нашему же ack.
//
// ponytail: fsync каталога не делаем — потолок известный, при полной строгости
// добавить sync на директорию после rename.
func writeFileAtomic(path string, src io.Reader) (int64, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()

	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op, если rename уже прошёл
	}()

	written, err := io.Copy(tmp, src)
	if err != nil {
		return 0, err
	}

	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}

	if err := os.Chmod(tmpName, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, err
	}

	return written, nil
}
