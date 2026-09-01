-- +goose Up

-- ============================================================================
-- НАЧАЛЬНАЯ СХЕМА МИКРОСЕРВИСА ГОЛОСОВОЙ ПОЧТЫ
--
-- Одна таблица: обращения, забранные с vmapi Asterisk, плюс наши поля —
-- статус и комментарий администратора, которых у АТС нет в принципе.
--
-- id        — свой сквозной идентификатор, им адресуются все HTTP-ручки.
-- record_id — идентификатор записи в vmapi (<epoch>-<msgXXXX>). Уникален
--             только внутри ящика, поэтому UNIQUE ставится на пару с mailbox.
--             Нужен для ack на АТС и для имени mp3-файла.
--
-- Времена — unix seconds INTEGER: в SQLite типов дат нет, а строки
-- сравниваются лексикографически и однажды подведут.
-- ============================================================================

CREATE TABLE messages (
    id              INTEGER PRIMARY KEY,            -- rowid; AUTOINCREMENT не нужен, строки не удаляем
    record_id       TEXT    NOT NULL,               -- id записи из vmapi
    mailbox         TEXT    NOT NULL,               -- 090, 051, ...
    mailbox_name    TEXT    NOT NULL DEFAULT '',    -- снапшот названия отдела на момент забора
    caller_number   TEXT    NOT NULL DEFAULT '',    -- пусто, если абонент скрыл номер
    caller_name     TEXT    NOT NULL DEFAULT '',
    received_at     INTEGER NOT NULL,               -- время звонка
    duration_sec    INTEGER NOT NULL DEFAULT 0,

    audio_path      TEXT,                           -- путь относительно AUDIO_DIR; NULL = запись ещё не скачана
    audio_bytes     INTEGER,
    audio_error     TEXT,                           -- причина, по которой звука нет

    status          TEXT    NOT NULL DEFAULT 'new'
                    CHECK (status IN ('new', 'in_progress', 'spam', 'done')),
    admin_comment   TEXT,

    updated_by_id   INTEGER,                        -- из заголовка X-User-Id (клейм id в JWT)
    updated_by_name TEXT,                           -- ФИО из тела запроса, иначе логин
    updated_at      INTEGER,

    fetched_at      INTEGER NOT NULL,               -- когда запись легла к нам
    acked_at        INTEGER,                        -- когда подтвердили АТС; NULL = ещё висит в INBOX

    UNIQUE (mailbox, record_id)
);

-- Единственный индекс: сортировка по умолчанию на каждой загрузке списка.
-- Фильтры по статусу и ящику отработают сканом — при десятках тысяч строк
-- это единицы миллисекунд. Добавлять индексы под них, только если упрёмся.
CREATE INDEX idx_messages_received_at ON messages (received_at DESC);

-- +goose Down

DROP TABLE messages;
