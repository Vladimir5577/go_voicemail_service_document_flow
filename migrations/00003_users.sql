-- +goose Up

-- ============================================================================
-- СПРАВОЧНИК ПОЛЬЗОВАТЕЛЕЙ
--
-- Не копия кадровой базы портала: сюда попадают только те, кто реально пришёл
-- в модуль. Пришёл человек — смотрим по id, нет такого — вставляем, есть —
-- ничего не делаем. Обновления на каждом запросе нет намеренно: в обычном
-- случае это одно чтение по первичному ключу, запись случается один раз за всё
-- время жизни пользователя.
--
-- Имя раньше лежало снепшотом в каждой строке — и в messages.updated_by_name,
-- и в message_plays.user_name. При ожидаемых сотнях тысяч прослушиваний в год
-- это лишние десятки мегабайт одинаковых строк. Здесь имя хранится по разу.
--
-- Плата за это — имя перестаёт быть историческим: журнал показывает текущее,
-- а не то, что было на момент действия. Принято сознательно, у пользователей
-- портала имена не меняются.
-- ============================================================================

CREATE TABLE users (
    id      INTEGER PRIMARY KEY,       -- id из JWT портала, свой не заводим
    name    TEXT    NOT NULL,          -- ФИО, если фронт его прислал, иначе логин
    seen_at INTEGER NOT NULL           -- когда впервые пришёл в модуль
);

-- Засыпка из того, что уже накоплено снепшотами. INSERT OR IGNORE, потому что
-- один и тот же человек мог править несколько обращений.
INSERT OR IGNORE INTO users (id, name, seen_at)
SELECT updated_by_id, updated_by_name, COALESCE(updated_at, strftime('%s', 'now'))
FROM messages
WHERE updated_by_id IS NOT NULL
  AND updated_by_name IS NOT NULL
  AND updated_by_name != '';

-- Колонки со снепшотами больше не нужны: имя берётся из справочника.
--
-- message_plays.user_name дропается здесь, а не убирается из 00002, потому что
-- на момент написания неизвестно, накатана ли 00002 на проде. Так миграция
-- отработает в любом случае. Если 00002 нигде не применялась — можно вместо
-- этого убрать колонку прямо из неё и выкинуть отсюда эту строку.
ALTER TABLE message_plays DROP COLUMN user_name;
ALTER TABLE messages      DROP COLUMN updated_by_name;

-- +goose Down

ALTER TABLE messages      ADD COLUMN updated_by_name TEXT;
ALTER TABLE message_plays ADD COLUMN user_name TEXT NOT NULL DEFAULT '';

UPDATE messages SET updated_by_name =
    (SELECT name FROM users WHERE users.id = messages.updated_by_id);
UPDATE message_plays SET user_name =
    COALESCE((SELECT name FROM users WHERE users.id = message_plays.user_id), '');

DROP TABLE users;
