-- Единая память (спека §8–§10, таблицы §18): provenance, entities, items,
-- связи, источники, производность, stances. Scope-изоляция:
-- public | chat:<id> | internal (§9.1).

-- provenance: разрешения и происхождение, назначаемые runtime (§18).
-- Модель предлагает связи с источниками, но не назначает себе разрешения (§9.1);
-- missing/invalid provenance трактуется как internal (§9.1).
CREATE TABLE provenance (
    id          bigserial PRIMARY KEY,
    kind        text        NOT NULL,   -- journal_event | web_document | runtime_action | persona_version
    ref         text        NOT NULL,   -- стабильная ссылка: event:17 / turn:42 / url:... / persona:3
    scopes      text[]      NOT NULL DEFAULT '{}', -- разрешённые аудитории производного
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX provenance_kind_ref_idx ON provenance (kind, ref);

-- entities: идентификатор предмета памяти (§8.1). Описания и оценки живут
-- записями memory_items, не в карточке entity.
CREATE TABLE entities (
    id              bigserial PRIMARY KEY,
    kind            text        NOT NULL, -- person | topic | place | object | event | project
    canonical_name  text        NOT NULL,
    scope_id        text        NOT NULL DEFAULT 'internal',
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX entities_scope_name_idx ON entities (scope_id, canonical_name);

-- entity_aliases: имена и алиасы со своей областью доступа (§8.1);
-- глобальной уникальности имени нет; алиас не даёт права доступа (§18).
CREATE TABLE entity_aliases (
    id          bigserial PRIMARY KEY,
    entity_id   bigint      NOT NULL REFERENCES entities(id),
    alias       text        NOT NULL,
    scope_id    text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX entity_aliases_scope_alias_idx ON entity_aliases (scope_id, alias);

-- sources: тип источника и стабильная ссылка (§8.3); deletion_status —
-- основа политики §19 (исключить текст из нового контекста).
CREATE TABLE sources (
    id               bigserial PRIMARY KEY,
    kind             text        NOT NULL,                  -- journal_event | web_document | runtime_action | persona_version
    ref              text        NOT NULL,
    metadata         jsonb       NOT NULL DEFAULT '{}',
    deletion_status  text        NOT NULL DEFAULT 'active', -- active | deleted
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX sources_kind_ref_idx ON sources (kind, ref);

-- memory_items: типизированные записи единой памяти (§8.2).
-- provenance_id обязателен: записи без разрешения не существуют (§9.1).
CREATE TABLE memory_items (
    id             bigserial PRIMARY KEY,
    kind           text        NOT NULL,                   -- claim | experience | thought | preference
    text           text        NOT NULL,
    scope_id       text        NOT NULL DEFAULT 'internal',
    provenance_id  bigint      NOT NULL REFERENCES provenance(id),
    status         text        NOT NULL DEFAULT 'active',  -- active | disputed | superseded | retracted
    salience       numeric     NOT NULL DEFAULT 0.5,       -- 0..1: значимость, не истинность
    confidence     numeric,                                -- только для проверяемых claim
    occurred_at    timestamptz,
    recorded_at    timestamptz NOT NULL DEFAULT now(),
    valid_from     timestamptz,
    valid_until    timestamptz,
    supersedes_id  bigint                                  -- пересмотр: старое → disputed/superseded (§10.3)
);

CREATE INDEX memory_items_scope_status_time_idx ON memory_items (scope_id, status, recorded_at); -- §18: items по scope/status/time
CREATE INDEX memory_items_fts_idx ON memory_items USING gin (to_tsvector('russian', text)); -- §18: full-text index

-- memory_links: одна запись — несколько объектов с ролью (§8.3):
-- событие не копируется в карточку каждого участника.
CREATE TABLE memory_links (
    memory_id  bigint NOT NULL REFERENCES memory_items(id),
    entity_id  bigint NOT NULL REFERENCES entities(id),
    role       text   NOT NULL DEFAULT 'subject' -- subject | participant | place | context
);

CREATE INDEX memory_links_entity_memory_idx ON memory_links (entity_id, memory_id); -- §18: memory links по entity
CREATE INDEX memory_links_memory_idx        ON memory_links (memory_id);

-- memory_sources: основания записи — источники (§8.3).
CREATE TABLE memory_sources (
    memory_id  bigint NOT NULL REFERENCES memory_items(id),
    source_id  bigint NOT NULL REFERENCES sources(id)
);

CREATE UNIQUE INDEX memory_sources_pair_idx ON memory_sources (memory_id, source_id);

-- memory_derivations: производность записей (§8.3) — консолидация
-- не делает источник снова активным и не превращает предположение в факт (§10.2).
CREATE TABLE memory_derivations (
    derived_id       bigint NOT NULL REFERENCES memory_items(id),
    input_memory_id  bigint NOT NULL REFERENCES memory_items(id)
);

CREATE UNIQUE INDEX memory_derivations_pair_idx ON memory_derivations (derived_id, input_memory_id);

-- stances: компактное текущее отношение (§8.4). Противоречивые впечатления
-- допустимы: несколько аспектов одной сущности. revision растёт при пересмотре;
-- основания — отдельными связями stance_basis, предыдущие не стираются.
CREATE TABLE stances (
    id          bigserial PRIMARY KEY,
    entity_id   bigint      NOT NULL REFERENCES entities(id),
    aspect      text        NOT NULL DEFAULT 'general',
    scope_id    text        NOT NULL,
    summary     text        NOT NULL,
    strength    numeric     NOT NULL DEFAULT 0.3, -- 0..1: устойчивость позиции, не истинность
    interest    numeric     NOT NULL DEFAULT 0.3, -- 0..1: желание возвращаться к теме
    revision    integer     NOT NULL DEFAULT 0,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX stances_entity_aspect_scope_idx ON stances (entity_id, aspect, scope_id);

CREATE TABLE stance_basis (
    stance_id  bigint NOT NULL REFERENCES stances(id),
    memory_id  bigint NOT NULL REFERENCES memory_items(id)
);

CREATE UNIQUE INDEX stance_basis_pair_idx ON stance_basis (stance_id, memory_id);
