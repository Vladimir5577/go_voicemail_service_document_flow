package service

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestAudioRelPath(t *testing.T) {
	received := time.Date(2026, 8, 24, 20, 6, 11, 0, time.UTC)

	got, err := audioRelPath("090", "1787591171-00000002", received)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}

	want := "2026/08/090-1787591171-00000002.mp3"
	if got != want {
		t.Errorf("путь = %q, ожидался %q", got, want)
	}
}

// Значения приходят с внешнего сервиса и идут прямо в имя файла: без проверки
// такой record_id записал бы mp3 в произвольное место на диске.
func TestAudioRelPathRejectsTraversal(t *testing.T) {
	received := time.Date(2026, 8, 24, 20, 6, 11, 0, time.UTC)

	cases := []struct{ mailbox, recordID string }{
		{"090", "../../etc/cron.d/x"},
		{"../090", "1787591171-00000002"},
		{"090", "1787591171/00000002"},
		{"090", ""},
		{"090", "id with spaces"},
	}

	for _, c := range cases {
		if got, err := audioRelPath(c.mailbox, c.recordID, received); err == nil {
			t.Errorf("audioRelPath(%q, %q) прошло и вернуло %q, ожидался отказ",
				c.mailbox, c.recordID, got)
		}
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/2026/08/090-1.mp3"

	written, err := writeFileAtomic(path, strings.NewReader("mp3-data"))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if written != 8 {
		t.Errorf("записано %d байт, ожидалось 8", written)
	}

	// Временных файлов после успешной записи остаться не должно.
	entries, err := os.ReadDir(dir + "/2026/08")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "090-1.mp3" {
		t.Errorf("в каталоге %v, ожидался только итоговый файл", entries)
	}
}
