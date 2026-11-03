-- Скука (этап 23.1, решение владельца 2026-11-03, §8): четвёртая шкала
-- состояния — внутреннее давление, которое растёт без событий и толкает к
-- ВНУТРЕННЕЙ активности (поиск интереса), но само сообщений не порождает.
-- Значение хранится в character_state (singleton id=1), рост считается
-- детерминированно от boredom_updated_at — время последнего «события»
-- (входящий разговор, применённый explore). В тихих блоках расписания не
-- растёт (этап 22.4): считает runtime, а не модель.
ALTER TABLE character_state
    ADD COLUMN boredom            numeric     NOT NULL DEFAULT 0 CHECK (boredom >= 0 AND boredom <= 1),
    ADD COLUMN boredom_updated_at timestamptz NOT NULL DEFAULT now();

COMMENT ON COLUMN character_state.boredom IS 'скука 0..1 (этап 23.1): растёт без событий, падает от разговора/находки';
COMMENT ON COLUMN character_state.boredom_updated_at IS 'последнее событие скуки (разговор/находка) или последняя зафиксированная точка роста — база расчёта';
