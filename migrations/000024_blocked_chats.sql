-- Заблокировавшие бота чаты (этап 38.7, живой прогон 2026-11-06):
-- 02:00:10 delivery получил 403 «Forbidden: bot was blocked by the user» на
-- фоновый check_in — чат продолжал жить в очереди намерений и инициатив.
-- Пометка снимается любым новым входящим сообщением этого чата (ingest).

CREATE TABLE blocked_chats (
    chat_id    bigint      PRIMARY KEY,
    reason     text        NOT NULL,
    blocked_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
