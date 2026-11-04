-- Этап 36: веб в разговорном ходе и картинки в чате.
--
-- web_usage — суточный учёт поисков разговорного пула (36.1): лимит §16 —
-- часть бюджета, поэтому счётчик в базе (философия №2), а не в памяти
-- процесса; сутки — UTC, как у budget_counters.
CREATE TABLE web_usage (
    kind       text        NOT NULL,                -- search
    period     date        NOT NULL,
    count      bigint      NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, period)
);

-- outbox_images — байты картинки хода (36.2): скачаны через fetch-слой §15 до
-- транзакции применения и лежат рядом со строкой доставки, поэтому рестарт и
-- retry sendPhoto не требуют повторного скачивания (и не зависят от того, что
-- по URL сейчас отдают). Ретенция outbox уносит картинки каскадом.
CREATE TABLE outbox_images (
    outbox_id    bigint      PRIMARY KEY REFERENCES delivery_outbox (id) ON DELETE CASCADE,
    content_type text        NOT NULL,               -- image/jpeg | image/png | image/webp | image/gif
    bytes        bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
