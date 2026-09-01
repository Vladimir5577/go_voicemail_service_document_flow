package service

import (
	"fmt"
	"time"
)

// NextRun возвращает ближайший момент после now среди времён вида "03:30".
// Если на сегодня все прошли — берётся первое время следующих суток.
func NextRun(now time.Time, at []string) (time.Time, error) {
	if len(at) == 0 {
		return time.Time{}, fmt.Errorf("расписание пустое")
	}

	var best time.Time
	for _, raw := range at {
		parsed, err := time.Parse("15:04", raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("не разобрать время %q: %w", raw, err)
		}

		candidate := time.Date(now.Year(), now.Month(), now.Day(),
			parsed.Hour(), parsed.Minute(), 0, 0, now.Location())
		if !candidate.After(now) {
			candidate = candidate.AddDate(0, 0, 1)
		}

		if best.IsZero() || candidate.Before(best) {
			best = candidate
		}
	}

	return best, nil
}
