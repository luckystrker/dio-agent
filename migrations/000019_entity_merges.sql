-- 32 (2026-11-03): объединение сущностей (§8.5, решение владельца).
-- Модель предлагает «A = B» с основанием, runtime проверяет и склеивает одной
-- транзакцией. Поглощённая сущность не удаляется: у неё остаётся merged_into,
-- а журнал entity_merges помнит снимок перенесённых строк для отката.

ALTER TABLE entities
    ADD COLUMN merged_into bigint REFERENCES entities(id);

CREATE INDEX entities_merged_into_idx ON entities (merged_into)
    WHERE merged_into IS NOT NULL;

-- entity_merges: журнал склеек. snapshot — JSON снимок перенесённых/удалённых
-- строк (алиасы, связи памяти, stances с основаниями, отношения), по нему
-- CLI-подкоманда entity-unmerge возвращает состояние как было. rolled_back_at
-- гасит активную запись: у одной поглощённой сущности не больше одной активной
-- склейки.
CREATE TABLE entity_merges (
    id              bigserial PRIMARY KEY,
    keep_id         bigint      NOT NULL REFERENCES entities(id),
    merge_id        bigint      NOT NULL REFERENCES entities(id),
    kind            text        NOT NULL,
    scope_id        text        NOT NULL,
    canonical_name  text        NOT NULL,
    reason          text        NOT NULL,
    proposed_by     text        NOT NULL, -- consolidation | operator
    snapshot        jsonb       NOT NULL,
    merged_at       timestamptz NOT NULL DEFAULT now(),
    rolled_back_at  timestamptz
);

CREATE UNIQUE INDEX entity_merges_active_merge_uidx ON entity_merges (merge_id)
    WHERE rolled_back_at IS NULL;
CREATE INDEX entity_merges_keep_idx ON entity_merges (keep_id, merged_at DESC);
