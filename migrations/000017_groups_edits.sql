-- Этап 30: группы и правки сообщений.
--
-- social_events.direct — событие прямо адресовано боту (§6.3: reply боту,
-- @бот, команда или личка). По нему выбирается приоритет хода (§5.2) и
-- решается судьба пачки группы после закрытия окна (30.1: прямое обращение
-- идёт в модель всегда, пассивный шум — по дешёвому фильтру).
ALTER TABLE social_events ADD COLUMN direct boolean NOT NULL DEFAULT false;
CREATE INDEX social_events_turn_direct_idx ON social_events (turn_id) WHERE direct;

-- chat_involvement — вовлечённость группового чата (§6.2/30.2): после того
-- как бот ответил или поставил реакцию, 5 минут каждая пачка идёт в модель;
-- любое сообщение продлевает окно, но не дальше потолка 30 минут подряд без
-- прямого обращения (прямое обращение сбрасывает потолок). last_unprompted_at
-- — потолок «встреваний» без прямого обращения (30.1: не чаще раза в 20 мин).
CREATE TABLE chat_involvement (
    chat_id            bigint PRIMARY KEY,
    involved_until     timestamptz,
    ceiling_until      timestamptz,
    last_unprompted_at timestamptz,
    updated_at         timestamptz NOT NULL DEFAULT now()
);
