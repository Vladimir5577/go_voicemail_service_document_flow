package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("Не удалось записать ответ", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// pagination — конверт списка, как у соседних модулей портала:
// {"items": [...], "pagination": {"page", "limit", "total", "pages"}}.
type pagination struct {
	Page  int `json:"page"`
	Limit int `json:"limit"`
	Total int `json:"total"`
	Pages int `json:"pages"`
}

func newPagination(page, limit, total int) pagination {
	pages := 0
	if limit > 0 {
		pages = (total + limit - 1) / limit
	}
	return pagination{Page: page, Limit: limit, Total: total, Pages: pages}
}
