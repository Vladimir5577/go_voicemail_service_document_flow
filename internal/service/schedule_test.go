package service

import (
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	cases := []struct {
		name string
		now  string
		at   []string
		want string
	}{
		{"ближайшее сегодня", "2026-08-31T01:00:00Z", []string{"03:30"}, "2026-08-31T03:30:00Z"},
		{"время прошло — завтра", "2026-08-31T04:00:00Z", []string{"03:30"}, "2026-09-01T03:30:00Z"},
		{"ровно в момент — завтра", "2026-08-31T03:30:00Z", []string{"03:30"}, "2026-09-01T03:30:00Z"},
		{"два времени, берём ближайшее", "2026-08-31T04:00:00Z", []string{"03:30", "13:00"}, "2026-08-31T13:00:00Z"},
		{"два времени, оба прошли", "2026-08-31T23:00:00Z", []string{"03:30", "13:00"}, "2026-09-01T03:30:00Z"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, c.now)
			if err != nil {
				t.Fatal(err)
			}

			got, err := NextRun(now, c.at)
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}

			if got.Format(time.RFC3339) != c.want {
				t.Errorf("NextRun = %s, ожидалось %s", got.Format(time.RFC3339), c.want)
			}
		})
	}
}

func TestNextRunRejectsGarbage(t *testing.T) {
	now := time.Now()

	if _, err := NextRun(now, nil); err == nil {
		t.Error("пустое расписание должно возвращать ошибку")
	}
	if _, err := NextRun(now, []string{"25:99"}); err == nil {
		t.Error("некорректное время должно возвращать ошибку")
	}
}
