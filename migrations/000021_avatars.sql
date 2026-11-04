-- Этап 34.2: аватарки собеседников.
--
-- person_avatars — состояние по (пользователь, скоуп чата): последний
-- увиденный file_unique_id, видимость аватарки и время следующей проверки.
-- Описание кладётся в память (experience) в скоупе чата §9.2, здесь — только
-- отметка «когда смотрели/что видели», чтобы не качать на каждом ходе.
-- next_check_at — очередь фоновой проверки (первый разговор, затем не чаще
-- раза в сутки); checked_at NULL — аватарку ещё ни разу не смотрели.
CREATE TABLE person_avatars (
    user_id        bigint      NOT NULL,
    scope_id       text        NOT NULL,
    chat_id        bigint      NOT NULL,
    actor_name     text,
    file_unique_id text,
    visible        boolean     NOT NULL DEFAULT true,
    description    text,
    checked_at     timestamptz,
    next_check_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, scope_id)
);

CREATE INDEX person_avatars_due_idx ON person_avatars (next_check_at);
