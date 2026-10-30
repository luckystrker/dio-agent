-- Персонаж (спека §7, таблица §18): версии персоны, компактное состояние,
-- отношения с людьми.

-- personas: версионированные записи персоны (§7.1) — вспомогательная таблица
-- к character_state: config/persona.md и config/system-prompt.md — источник
-- истины; отличие файла от активной записи создаёт новую версию, история
-- сохраняется. mood_* здесь — baseline для затухания (§7.2).
CREATE TABLE personas (
    id                 bigserial PRIMARY KEY,
    version            integer     NOT NULL UNIQUE,
    active             boolean     NOT NULL DEFAULT true,
    name               text        NOT NULL,
    description        text        NOT NULL,
    speech_style       text        NOT NULL,
    values             jsonb       NOT NULL DEFAULT '[]',
    mood_valence       numeric     NOT NULL DEFAULT 0,
    mood_arousal       numeric     NOT NULL DEFAULT 0.4,
    mood_social_energy numeric     NOT NULL DEFAULT 0.6,
    -- persona_seed — художественная биография, помечена как вымысел (§7.1)
    seed_bio           text,
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- character_state: singleton id=1 (§18): активная версия персоны, mood (§7.2),
-- ревизии контекста/удалений (§13.3, §19). Физическое mood-состояние — здесь,
-- затухание к baseline вычисляется при чтении (§7.2).
CREATE TABLE character_state (
    id                 integer PRIMARY KEY CHECK (id = 1),
    persona_version    integer     NOT NULL DEFAULT 1 REFERENCES personas(version),
    valence            numeric     NOT NULL DEFAULT 0,        -- -1..1
    arousal            numeric     NOT NULL DEFAULT 0.4,      -- 0..1
    social_energy      numeric     NOT NULL DEFAULT 0.6,      -- 0..1
    context_revision   bigint      NOT NULL DEFAULT 0,
    deletion_revision  bigint      NOT NULL DEFAULT 0,
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- relationships: числовые регуляторы по человеку в scope чата (§7.3);
-- основания изменений — записи памяти со ссылками на источники, не здесь.
CREATE TABLE relationships (
    person_entity_id  bigint      NOT NULL REFERENCES entities(id),
    scope_id          text        NOT NULL,
    familiarity       numeric     NOT NULL DEFAULT 0,   -- 0..1
    affinity          numeric     NOT NULL DEFAULT 0,   -- -1..1
    trust             numeric     NOT NULL DEFAULT 0.2, -- 0..1
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (person_entity_id, scope_id)
);
