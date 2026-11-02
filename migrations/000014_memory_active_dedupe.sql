-- Точная дедупликация активных записей памяти (блок 18.8): дубли копились
-- как active с равным весом («поехал делать ремонт с женой» и «поехал с женой
-- делать ремонт» — обе в выдаче). Уникальный индекс по выражению
-- (scope_id, md5(lower(btrim(text)))) WHERE status='active' останавливает
-- рост: повторная вставка идентичного текста уходит в ON CONFLICT DO NOTHING
-- (memory.Items.Insert — честный пропуск, без ошибки).
--
-- Существующие точные дубли схлопываются в старейшую строку (решение
-- владельца 2026-11-02: память — производное от журнала, журнал хранит всё,
-- точный дубликат текста знания не несёт). Перед удалением младшего дубля его
-- ссылки, которых у старейшего ещё нет, ПЕРЕНОСЯТСЯ на старейший — удаление
-- дубля не теряет источники и связи. Цепочки пересмотра Supersede не
-- строятся (мёртвый код Supersede/Retract — бэклог, блоком не трогается).

-- 1. Перенос ссылок младших дублей на старейшие строки их групп.

-- memory_links без UNIQUE-ограничения — перенос гвардится NOT EXISTS.
WITH dupes AS (
    SELECT m.id AS doomed_id, keep.id AS keep_id
    FROM memory_items m
    JOIN memory_items keep
      ON keep.id < m.id
     AND keep.scope_id = m.scope_id
     AND keep.status = 'active'
     AND m.status = 'active'
     AND md5(lower(btrim(keep.text))) = md5(lower(btrim(m.text)))
)
INSERT INTO memory_links (memory_id, entity_id, role)
SELECT d.keep_id, l.entity_id, l.role
FROM memory_links l
JOIN dupes d ON d.doomed_id = l.memory_id
WHERE NOT EXISTS (
    SELECT 1 FROM memory_links kl
    WHERE kl.memory_id = d.keep_id
      AND kl.entity_id = l.entity_id
      AND kl.role = l.role);

WITH dupes AS (
    SELECT m.id AS doomed_id, keep.id AS keep_id
    FROM memory_items m
    JOIN memory_items keep
      ON keep.id < m.id
     AND keep.scope_id = m.scope_id
     AND keep.status = 'active'
     AND m.status = 'active'
     AND md5(lower(btrim(keep.text))) = md5(lower(btrim(m.text)))
)
INSERT INTO memory_sources (memory_id, source_id)
SELECT d.keep_id, s.source_id
FROM memory_sources s
JOIN dupes d ON d.doomed_id = s.memory_id
ON CONFLICT (memory_id, source_id) DO NOTHING;

WITH dupes AS (
    SELECT m.id AS doomed_id, keep.id AS keep_id
    FROM memory_items m
    JOIN memory_items keep
      ON keep.id < m.id
     AND keep.scope_id = m.scope_id
     AND keep.status = 'active'
     AND m.status = 'active'
     AND md5(lower(btrim(keep.text))) = md5(lower(btrim(m.text)))
)
INSERT INTO memory_derivations (derived_id, input_memory_id)
SELECT d.keep_id, dv.input_memory_id
FROM memory_derivations dv
JOIN dupes d ON d.doomed_id = dv.derived_id
ON CONFLICT (derived_id, input_memory_id) DO NOTHING;

-- stance_basis ссылается на запись-основание: перепривязка к старейшей.
WITH dupes AS (
    SELECT m.id AS doomed_id, keep.id AS keep_id
    FROM memory_items m
    JOIN memory_items keep
      ON keep.id < m.id
     AND keep.scope_id = m.scope_id
     AND keep.status = 'active'
     AND m.status = 'active'
     AND md5(lower(btrim(keep.text))) = md5(lower(btrim(m.text)))
)
INSERT INTO stance_basis (stance_id, memory_id)
SELECT sb.stance_id, d.keep_id
FROM stance_basis sb
JOIN dupes d ON d.doomed_id = sb.memory_id
ON CONFLICT (stance_id, memory_id) DO NOTHING;

-- 2. Удаление опустевших младших дублей вместе с их оставшимися ссылками
--    (оставшиеся — те, что уже есть у старейшего: дубликат ссылки не нужен).

WITH dupes AS (
    SELECT m.id
    FROM memory_items m
    JOIN memory_items keep
      ON keep.id < m.id
     AND keep.scope_id = m.scope_id
     AND keep.status = 'active'
     AND m.status = 'active'
     AND md5(lower(btrim(keep.text))) = md5(lower(btrim(m.text)))
)
DELETE FROM memory_links
WHERE memory_id IN (SELECT id FROM dupes);

WITH dupes AS (
    SELECT m.id
    FROM memory_items m
    JOIN memory_items keep
      ON keep.id < m.id
     AND keep.scope_id = m.scope_id
     AND keep.status = 'active'
     AND m.status = 'active'
     AND md5(lower(btrim(keep.text))) = md5(lower(btrim(m.text)))
)
DELETE FROM memory_sources
WHERE memory_id IN (SELECT id FROM dupes);

WITH dupes AS (
    SELECT m.id
    FROM memory_items m
    JOIN memory_items keep
      ON keep.id < m.id
     AND keep.scope_id = m.scope_id
     AND keep.status = 'active'
     AND m.status = 'active'
     AND md5(lower(btrim(keep.text))) = md5(lower(btrim(m.text)))
)
DELETE FROM memory_derivations
WHERE derived_id IN (SELECT id FROM dupes)
   OR input_memory_id IN (SELECT id FROM dupes);

WITH dupes AS (
    SELECT m.id
    FROM memory_items m
    JOIN memory_items keep
      ON keep.id < m.id
     AND keep.scope_id = m.scope_id
     AND keep.status = 'active'
     AND m.status = 'active'
     AND md5(lower(btrim(keep.text))) = md5(lower(btrim(m.text)))
)
DELETE FROM stance_basis
WHERE memory_id IN (SELECT id FROM dupes);

WITH dupes AS (
    SELECT m.id
    FROM memory_items m
    JOIN memory_items keep
      ON keep.id < m.id
     AND keep.scope_id = m.scope_id
     AND keep.status = 'active'
     AND m.status = 'active'
     AND md5(lower(btrim(keep.text))) = md5(lower(btrim(m.text)))
)
DELETE FROM memory_items
WHERE id IN (SELECT id FROM dupes);

-- 3. Барьер для будущих вставок (для любого пишущего, не только Items.Insert).
CREATE UNIQUE INDEX memory_items_active_dedupe_uidx
    ON memory_items (scope_id, md5(lower(btrim(text))))
    WHERE status = 'active';
