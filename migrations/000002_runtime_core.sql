-- Ядро runtime (спека §18; поведение — §6, §12, §13):
-- журнал событий Telegram, ходы, outbox доставки, общее состояние процесса,
-- событийный журнал ago и чекпоинты фоновых задач.

-- social_events: Telegram update/message версии (§6.1).
-- UNIQUE(telegram_update_id) — дедупликация пачек: повторная пачка = «уже сохранено», не ошибка.
-- Правки сообщений: telegram_message_id + version связывают версии edit'ов
-- (§19); уникальность message/event/version задаёт нормализатор — придёт
-- вместе с обработкой edit/delete, пока фиксируем данные.
CREATE TABLE social_events (
    id                  bigserial PRIMARY KEY,
    telegram_update_id  bigint      NOT NULL UNIQUE,
    chat_id             bigint      NOT NULL,
    chat_type           text        NOT NULL,                   -- private | group
    actor_id            bigint,
    actor_name          text,
    kind                text        NOT NULL,                   -- message | reply | mention | command | edited | other
    text                text,
    raw_update          jsonb       NOT NULL,
    status              text        NOT NULL DEFAULT 'pending', -- pending | journal_only | buffered | processed
    turn_id             bigint,
    telegram_message_id bigint,
    version             integer     NOT NULL DEFAULT 0,         -- 0 = исходное сообщение, N = номер правки
    occurred_at         timestamptz NOT NULL DEFAULT now(),
    summarized_at       timestamptz                             -- §10.2: событие вошло в episode summary
);

CREATE INDEX social_events_status_chat_idx ON social_events (status, chat_id);
CREATE INDEX social_events_chat_time_idx   ON social_events (chat_id, occurred_at); -- §18: events по chat/time
CREATE INDEX social_events_turn_idx        ON social_events (turn_id);
CREATE INDEX social_events_message_idx     ON social_events (chat_id, telegram_message_id);

-- turns: стабильный ID batch (§6.2) — переживает потерю таймера.
-- Однократность применения гарантирует событийный журнал (§12.1, инвариант 13);
-- status/attempts/applied_at — служебная видимость для recovery-таблицы §12.2 и оператора.
CREATE TABLE turns (
    id                bigserial PRIMARY KEY,
    chat_id           bigint      NOT NULL,
    status            text        NOT NULL DEFAULT 'created', -- created | applied | failed | skipped
    attempts          integer     NOT NULL DEFAULT 0,
    validated_result  jsonb,                                  -- §12.2: результат переживает процесс
    context_revision  bigint      NOT NULL DEFAULT 0,
    applied_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX turns_chat_created_idx ON turns (chat_id, created_at);

-- delivery_outbox: единицы доставки (§13.1). UNIQUE(turn_id, position) —
-- внутренний ключ против дублей намерений отправки; exactly-once для внешнего
-- API не заявляется (§13.2).
CREATE TABLE delivery_outbox (
    id                   bigserial PRIMARY KEY,
    turn_id              bigint      NOT NULL,
    position             integer     NOT NULL,
    chat_id              bigint      NOT NULL,
    payload              jsonb       NOT NULL,
    not_before           timestamptz NOT NULL DEFAULT now(),
    status               text        NOT NULL DEFAULT 'pending', -- pending | sending | sent | retry_wait | unknown | cancelled | failed
    attempts             integer     NOT NULL DEFAULT 0,
    telegram_message_id  bigint,
    last_error           text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT delivery_outbox_turn_position_key UNIQUE (turn_id, position)
);

CREATE INDEX delivery_outbox_status_due_idx ON delivery_outbox (status, not_before); -- §18: pending work по status/due time
CREATE INDEX delivery_outbox_chat_order_idx ON delivery_outbox (chat_id, turn_id, position);

-- runtime_state: общие курсоры процесса (§18): poll offset (§6.1), config
-- version, maintenance cursor. Offset двигается только в той же транзакции,
-- что и запись события (§6.1, инвариант 2).
CREATE TABLE runtime_state (
    key         text PRIMARY KEY,
    value       jsonb       NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- event_streams: событийный журнал ходов и фоновых задач (§12.1) —
-- хранилище адаптера stream.Storage фреймворка ago (§17).
-- stream_id — идентификатор хода/задачи (в ago это строка), seq — номер по порядку;
-- UNIQUE(stream_id, seq) — однократность записи события, из неё выводится
-- однократность применения хода (инварианты 4 и 13).
CREATE TABLE event_streams (
    stream_id   text        NOT NULL,
    seq         bigint      NOT NULL,
    type        text        NOT NULL,                 -- turn_started | tool_call | tool_result | model_output | deltas_applied | delivery_queued | turn_skipped (§12.1) и события фоновых задач
    payload     jsonb       NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (stream_id, seq)
);

CREATE INDEX event_streams_created_idx ON event_streams (created_at);

-- Append-only (§18, инвариант 13): изменение и удаление запрещены триггером —
-- механика, а не дисциплина. Восстановление есть перечитывание журнала.
CREATE FUNCTION forbid_event_streams_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'event_streams — append-only журнал (§12.1): % запрещено', TG_OP;
END;
$$;

CREATE TRIGGER event_streams_append_only
    BEFORE UPDATE OR DELETE ON event_streams
    FOR EACH ROW EXECUTE FUNCTION forbid_event_streams_mutation();

-- task_checkpoints: чекпоинты фоновых задач — хранилище task.CheckpointStore
-- фреймворка ago (§12.2, §18). WAL задач ведётся в event_streams отдельными
-- потоками (stream = задача).
CREATE TABLE task_checkpoints (
    task_id     text        NOT NULL UNIQUE,
    state       jsonb       NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);
