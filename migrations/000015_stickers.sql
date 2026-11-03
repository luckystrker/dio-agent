-- Коллекция стикеров персонажа (этап 25.3, решение 2026-11-03).
-- Стартовый набор — секция «Стикеры» persona-файла (source='persona',
-- синхронизируется при старте), сохранённые персонажем — source='saved'
-- (25.5). file_unique_id может быть NULL у строк персоны: файл даёт только
-- file_id, уникальный id дорисовывается при первой встрече стикера в пачке.
-- Он и есть ключ дедупликации «сохраню себе» (частичный UNIQUE).
CREATE TABLE stickers (
    id             bigserial   PRIMARY KEY,
    file_id        text        NOT NULL,
    file_unique_id text,
    emoji          text        NOT NULL DEFAULT '',
    set_name       text        NOT NULL DEFAULT '',
    note           text        NOT NULL DEFAULT '',  -- «когда уместен» (для панели/выбора)
    source         text        NOT NULL DEFAULT 'saved' CHECK (source IN ('persona', 'saved')),
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Дубли «сохраню себе» отсекаются по file_unique_id; NULL (персона до
-- встречи) не участвует в уникальности — так допускает PostgreSQL.
CREATE UNIQUE INDEX stickers_file_unique_uidx ON stickers (file_unique_id)
    WHERE file_unique_id IS NOT NULL;

-- Строка персоны однозначна по file_id: повторная синхронизация обновляет её,
-- а не плодит дубли. Сохранённые строки этим индексом не ограничены.
CREATE UNIQUE INDEX stickers_persona_file_id_uidx ON stickers (file_id)
    WHERE source = 'persona';
