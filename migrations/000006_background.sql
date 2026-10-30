-- Фон (спека §14, таблица §18): единая модель намерений с dedupe и сроками.
-- Исполнимое намерение всегда имеет notBefore и expiresAt (§14.1); мысль без
-- будущего действия остаётся memory item.

CREATE TABLE intentions (
    id               bigserial PRIMARY KEY,
    kind             text        NOT NULL,                    -- reply_later | check_in | share | explore (§14.1)
    target_chat_id   bigint,                                  -- у explore может отсутствовать (§14.1 targetChatId?)
    topic            text        NOT NULL,
    entity_ids       jsonb       NOT NULL DEFAULT '[]',
    reason           text        NOT NULL,
    source_ids       jsonb       NOT NULL DEFAULT '[]',       -- ссылки на источники оснований (§14.1)
    provenance_id    bigint      NOT NULL REFERENCES provenance(id),
    priority         integer     NOT NULL DEFAULT 0,
    status           text        NOT NULL DEFAULT 'pending',  -- pending | running | completed | cancelled | failed (§14.1)
    not_before       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz,
    -- dedupe по типу+теме+чату: перефразировки не размножают действие (§14.1)
    dedupe_key       text        NOT NULL UNIQUE,
    related_turn_id  bigint,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX intentions_status_due_idx ON intentions (status, not_before); -- §18: pending work по status/due time
