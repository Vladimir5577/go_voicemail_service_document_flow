package model

import "time"

// Статусы обращения. Справочника в БД нет — их четыре и они зашиты в CHECK.
const (
	StatusNew        = "new"
	StatusInProgress = "in_progress"
	StatusSpam       = "spam"
	StatusDone       = "done"
)

func ValidStatus(s string) bool {
	switch s {
	case StatusNew, StatusInProgress, StatusSpam, StatusDone:
		return true
	}
	return false
}

// Message — обращение из голосового ящика. Нулевые значения означают NULL:
// пустой AudioPath — звук ещё не скачан, нулевой AckedAt — ещё не подтверждено АТС.
type Message struct {
	ID          int64
	RecordID    string
	Mailbox     string
	MailboxName string

	CallerNumber string
	CallerName   string
	ReceivedAt   time.Time
	DurationSec  int

	AudioPath  string
	AudioBytes int64
	AudioError string

	Status       string
	AdminComment string

	// PlayCount — сколько раз открывали запись. Считается на лету из message_plays,
	// своей колонки в messages нет: счётчик и журнал не должны расходиться.
	PlayCount int

	// Авторство раздельное: одно поле на двоих приписывало статус тому, кто на
	// самом деле правил только комментарий. Имена подтягиваются из users.
	StatusByID    int64
	StatusByName  string
	StatusAt      time.Time

	CommentByID   int64
	CommentByName string
	CommentAt     time.Time

	FetchedAt time.Time
	AckedAt   time.Time
}

func (m *Message) HasAudio() bool {
	return m.AudioPath != ""
}

// Play — отметка о том, что кто-то открыл запись разговора.
//
// UserName в таблице не хранится: он приезжает join-ом из users. В строке лежит
// только UserID — при сотнях тысяч прослушиваний в год дублировать имя в каждой
// накладно, а меняться оно не должно.
type Play struct {
	UserID   int64
	UserName string
	PlayedAt time.Time
}

// MailboxRef — строка справочника ящиков, собранного из самих обращений.
// Отдельной таблицы нет: за названиями не нужно ходить на АТС, а фильтр
// в интерфейсе должен работать, даже когда она недоступна.
type MailboxRef struct {
	Mailbox string
	Name    string
	Total   int
}
