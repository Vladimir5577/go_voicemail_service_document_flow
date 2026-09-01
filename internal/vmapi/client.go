// Package vmapi — клиент к сервису голосовой почты на самой АТС.
//
// Особенность upstream: он отдаёт списком только новые обращения (INBOX).
// Пока запись не подтверждена через ack, она приезжает снова и снова с тем же
// id — это и есть гарантия «хотя бы один раз», на которой держится вся
// сохранность данных. Листинга архива в API нет.
package vmapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

func New(baseURL, token string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		hc:      &http.Client{Timeout: timeout},
	}
}

// Error несёт HTTP-статус: 409 («ящик занят, АТС в него пишет») — штатная
// ситуация, которую нужно повторить, а не считать отказом.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("vmapi: код %d: %s", e.Status, e.Message)
}

type Mailbox struct {
	Mailbox string `json:"mailbox"`
	Name    string `json:"name"`
	New     int    `json:"new"`
	Old     int    `json:"old"`
}

type mailboxesResponse struct {
	TotalNew  int       `json:"total_new"`
	Mailboxes []Mailbox `json:"mailboxes"`
}

type Message struct {
	ID            string `json:"id"`
	Mailbox       string `json:"mailbox"`
	CallerNumber  string `json:"caller_number"`
	CallerName    string `json:"caller_name"`
	ReceivedEpoch int64  `json:"received_epoch"`
	DurationSec   int    `json:"duration_sec"`
	AudioError    string `json:"audio_error"`
}

type MessagesPage struct {
	Mailbox   string    `json:"mailbox"`
	Count     int       `json:"count"`
	Truncated bool      `json:"truncated"`
	Messages  []Message `json:"messages"`
}

type AckResult struct {
	Mailbox  string   `json:"mailbox"`
	Moved    []string `json:"moved"`
	NotFound []string `json:"not_found"`
}

func (c *Client) Mailboxes(ctx context.Context) ([]Mailbox, error) {
	var out mailboxesResponse
	if err := c.getJSON(ctx, "/api/v1/mailboxes", nil, &out); err != nil {
		return nil, err
	}
	return out.Mailboxes, nil
}

// Messages запрашивает новые обращения ящика без звука: base64 в списке раздул
// бы ответ на треть, а одна запись, которую не смог перекодировать ffmpeg,
// осталась бы без mp3 вместе со всей пачкой. Аудио забираем поштучно.
func (c *Client) Messages(ctx context.Context, mailbox string, limit int) (*MessagesPage, error) {
	query := url.Values{}
	query.Set("audio", "0")
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}

	var out MessagesPage
	path := "/api/v1/mailboxes/" + url.PathEscape(mailbox) + "/messages"
	if err := c.getJSON(ctx, path, query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Audio отдаёт поток mp3. Закрывать вызывающему.
func (c *Client) Audio(ctx context.Context, mailbox, recordID string) (io.ReadCloser, error) {
	path := "/api/v1/mailboxes/" + url.PathEscape(mailbox) +
		"/messages/" + url.PathEscape(recordID) + "/audio"

	resp, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Ack подтверждает обработку: записи уходят из новых в архив ящика.
//
// Передавать сюда можно только те id, что действительно сохранены у нас.
// Подтверждённое и не сохранённое восстановить будет неоткуда: метаданные
// (номер звонившего в первую очередь) живут только пока запись в INBOX.
func (c *Client) Ack(ctx context.Context, mailbox string, recordIDs []string) (*AckResult, error) {
	if len(recordIDs) == 0 {
		return &AckResult{Mailbox: mailbox}, nil
	}

	body, err := json.Marshal(map[string]any{"ids": recordIDs})
	if err != nil {
		return nil, err
	}

	path := "/api/v1/mailboxes/" + url.PathEscape(mailbox) + "/ack"

	// 409 означает, что Asterisk прямо сейчас пишет в каталог ящика. Штатная
	// ситуация, а не отказ: ждём и повторяем.
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 1500 * time.Millisecond):
			}
		}

		var out AckResult
		err := c.sendJSON(ctx, path, body, &out)
		if err == nil {
			return &out, nil
		}

		lastErr = err

		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
			break
		}
	}

	return nil, lastErr
}

func (c *Client) Health(ctx context.Context) error {
	var out map[string]any
	return c.getJSON(ctx, "/api/v1/health", nil, &out)
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) sendJSON(ctx context.Context, path string, body []byte, out any) error {
	resp, err := c.do(ctx, http.MethodPost, path, nil, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Response, error) {
	if c.baseURL == "" {
		return nil, &Error{Message: "VMAPI_URL не задан"}
	}

	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &Error{Message: err.Error()}
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, &Error{Status: resp.StatusCode, Message: readError(resp.Body)}
	}

	return resp, nil
}

// readError достаёт текст из {"error": "..."} — тело ошибки у vmapi всегда такое.
func readError(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil || len(raw) == 0 {
		return "нет ответа"
	}

	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Error != "" {
		return payload.Error
	}

	return string(raw)
}
