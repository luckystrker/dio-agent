-- Бюджеты (спека §16, таблицы §18): резервирование до модельного вызова —
-- middleware ago; check и reserve — одна транзакция (§16).
-- Уникальность operation_id: retry той же операции не резервирует повторно,
-- новая платная попытка получает отдельный operation_id с номером attempt.

CREATE TABLE budget_reservations (
    id            bigserial PRIMARY KEY,
    operation_id  text        NOT NULL UNIQUE,
    attempt       integer     NOT NULL DEFAULT 0,
    pool          text        NOT NULL,                -- conversation | background | maintenance (§16)
    reserved      jsonb       NOT NULL DEFAULT '{}',   -- зарезервированная оценка: токены/число вызовов/proactive count
    actual        jsonb,                               -- фактический usage; NULL — не зафиксирован (§16: неизвестная стоимость не нулевая)
    period        date        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX budget_reservations_pool_period_idx ON budget_reservations (pool, period);
CREATE INDEX budget_reservations_period_idx      ON budget_reservations (period);

-- budget_counters: агрегаты зарезервированного и фактического расхода
-- по пулу и периоду; сами лимиты задаются конфигурацией, не в БД (§16).
CREATE TABLE budget_counters (
    pool             text        NOT NULL,
    period           date        NOT NULL,
    reserved_tokens  bigint      NOT NULL DEFAULT 0,
    reserved_calls   bigint      NOT NULL DEFAULT 0,
    actual_tokens    bigint      NOT NULL DEFAULT 0,
    actual_calls     bigint      NOT NULL DEFAULT 0,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (pool, period)
);
