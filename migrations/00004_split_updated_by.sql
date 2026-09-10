-- +goose Up

-- ============================================================================
-- РАЗДЕЛЕНИЕ АВТОРСТВА: СТАТУС И КОММЕНТАРИЙ ОТДЕЛЬНО
--
-- Было одно поле на двоих: updated_by_id перезаписывался при любом PATCH,
-- что бы в нём ни меняли. Иванов ставил статус, Петров через три дня правил
-- комментарий — и обращение начинало утверждать, что статус поставил Петров.
-- Не «мы не знаем автора», а «мы называем не того».
--
-- Истории изменений здесь по-прежнему нет и не планируется: нужна не цепочка,
-- а верная подпись под каждым из двух полей. Если однажды понадобится цепочка,
-- эти колонки останутся закэшированным «сейчас» и никуда не денутся.
-- ============================================================================

ALTER TABLE messages ADD COLUMN status_by_id  INTEGER;
ALTER TABLE messages ADD COLUMN status_at     INTEGER;
ALTER TABLE messages ADD COLUMN comment_by_id INTEGER;
ALTER TABLE messages ADD COLUMN comment_at    INTEGER;

-- Засыпка приблизительная — точнее из одного поля не вытащить.
-- Статус приписываем последнему правившему, только если статус вообще уходил
-- от стартового 'new'; комментарий — только если он есть.
UPDATE messages SET
    status_by_id  = CASE WHEN status != 'new'          THEN updated_by_id END,
    status_at     = CASE WHEN status != 'new'          THEN updated_at    END,
    comment_by_id = CASE WHEN admin_comment IS NOT NULL THEN updated_by_id END,
    comment_at    = CASE WHEN admin_comment IS NOT NULL THEN updated_at    END
WHERE updated_by_id IS NOT NULL;

ALTER TABLE messages DROP COLUMN updated_by_id;
ALTER TABLE messages DROP COLUMN updated_at;

-- +goose Down

ALTER TABLE messages ADD COLUMN updated_by_id INTEGER;
ALTER TABLE messages ADD COLUMN updated_at    INTEGER;

-- Обратно схлопываем в одно поле по более свежей из двух отметок.
UPDATE messages SET
    updated_by_id = CASE
        WHEN COALESCE(comment_at, 0) > COALESCE(status_at, 0) THEN comment_by_id
        ELSE COALESCE(status_by_id, comment_by_id)
    END,
    updated_at = MAX(COALESCE(status_at, 0), COALESCE(comment_at, 0))
WHERE status_by_id IS NOT NULL OR comment_by_id IS NOT NULL;

ALTER TABLE messages DROP COLUMN status_by_id;
ALTER TABLE messages DROP COLUMN status_at;
ALTER TABLE messages DROP COLUMN comment_by_id;
ALTER TABLE messages DROP COLUMN comment_at;
