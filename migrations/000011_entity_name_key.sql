-- 17.12 (2026-11-02): нормализация имён сущностей и уникальность (scope, имя).
-- Поиск шёл по lower(canonical_name) без нормализации и без уникального
-- индекса: «Алёна»/«Алена», «Данила  К.»/«Данила К.» — разные сущности,
-- карточка и память делились, гонка двух ходов создавала дубль. name_key —
-- нормализованный ключ имени (сжатие пробелов, lower, ё→е), генерируется
-- базой при записи; уникальный индекс (scope_id, name_key) держит одно имя
-- на скоуп. Существующие дубли сводятся к строке min(id) с переносом всех
-- зависимостей (связи памяти, алиасы, stances с основаниями, relationships).

ALTER TABLE entities ADD COLUMN name_key text
  GENERATED ALWAYS AS (
    replace(lower(btrim(regexp_replace(normalize(canonical_name, NFC), '\s+', ' ', 'g'))), 'ё', 'е')
  ) STORED;

-- Дубли по (scope_id, name_key): каноническая строка — min(id).
CREATE TEMP TABLE entity_dups AS
SELECT e.id AS dup_id, min(e.id) OVER (PARTITION BY e.scope_id, e.name_key) AS keep_id
FROM entities e;
DELETE FROM entity_dups WHERE dup_id = keep_id;

-- relationships: PK (person_entity_id, scope_id); на пару (группа, scope)
-- остаётся один ряд — самый свежий. Дедуп идёт ДО переноса и по шагам:
-- подзапросы в одном операторе не видят строк, изменённых этим же оператором
-- (снимок оператора), поэтому щит EXISTS внутри одного UPDATE пропускал бы
-- второй дубль к той же паре — duplicate key pkey. Шаги:
-- 1) между самими дублями остаётся свежий (updated_at, затем ctid);
-- 2) каноническая строка пары проигрывает свежести дубликата — удаляется;
-- 3) оставшийся дубль при живой канонической строке (она свежее или равна)
--    удаляется;
-- 4) единственный ряд на пару переезжает на каноническую.
DELETE FROM relationships r
USING entity_dups d, entity_dups d2, relationships o
WHERE r.person_entity_id = d.dup_id
  AND o.person_entity_id = d2.dup_id AND d2.keep_id = d.keep_id
  AND o.scope_id = r.scope_id
  AND (o.updated_at, o.ctid) > (r.updated_at, r.ctid);
DELETE FROM relationships k
USING entity_dups d JOIN relationships r ON r.person_entity_id = d.dup_id
WHERE k.person_entity_id = d.keep_id AND k.scope_id = r.scope_id
  AND r.updated_at > k.updated_at;
DELETE FROM relationships r
USING entity_dups d
WHERE r.person_entity_id = d.dup_id
  AND EXISTS (SELECT 1 FROM relationships k
              WHERE k.person_entity_id = d.keep_id AND k.scope_id = r.scope_id);
UPDATE relationships r SET person_entity_id = d.keep_id
FROM entity_dups d WHERE r.person_entity_id = d.dup_id;

-- stances: пара (entity, aspect, scope) уникальна по индексу. Победитель
-- пары (aspect, scope) группы — строка канонической сущности, при её
-- отсутствии — самый свежий станс дубликатов (updated_at DESC, затем id);
-- одинаковый аспект у двух дублей сводится к победителю ДО переноса
-- (снимок оператора — та же причина, что и выше: NOT EXISTS внутри одного
-- UPDATE не видит уже перенесённую строку, оба станса уезжали на
-- каноническую и ломали индекс). Проигравшие отдают победителю основания
-- (повтор пары основания отбрасывается) и удаляются; победитель из
-- дубликатов затем переезжает на каноническую целиком.
CREATE TEMP TABLE stance_survivors AS
SELECT DISTINCT d.keep_id, s.aspect, s.scope_id,
       COALESCE(
         (SELECT k.id FROM stances k
          WHERE k.entity_id = d.keep_id AND k.aspect = s.aspect AND k.scope_id = s.scope_id),
         (SELECT w.id FROM stances w
          JOIN entity_dups d2 ON d2.dup_id = w.entity_id AND d2.keep_id = d.keep_id
          WHERE w.aspect = s.aspect AND w.scope_id = s.scope_id
          ORDER BY w.updated_at DESC, w.id LIMIT 1)
       ) AS winner_id
FROM stances s
JOIN entity_dups d ON d.dup_id = s.entity_id;

UPDATE stance_basis b
SET stance_id = sv.winner_id
FROM stance_survivors sv
JOIN entity_dups d ON d.keep_id = sv.keep_id
JOIN stances s ON s.entity_id = d.dup_id AND s.aspect = sv.aspect AND s.scope_id = sv.scope_id
WHERE b.stance_id = s.id
  AND b.stance_id <> sv.winner_id
  AND NOT EXISTS (SELECT 1 FROM stance_basis x
                  WHERE x.stance_id = sv.winner_id AND x.memory_id = b.memory_id)
  -- Снимок оператора не видит собственных правок: тот же memory_id у двух
  -- дублей одной пары проходил щит выше дважды (у победителя его ещё нет) и
  -- уезжал обеими строками — duplicate key stance_basis_pair_idx. Тай-брейк
  -- по ctid: забирает более ранняя строка, повтор остаётся на проигравшем
  -- стансе и уходит с ним (законный легаси-расклад: одна запись памяти была
  -- основанием стансов обоих дублей).
  AND NOT EXISTS (
    SELECT 1
    FROM stance_basis c
    JOIN stances s2 ON s2.id = c.stance_id
    JOIN entity_dups d2 ON d2.dup_id = s2.entity_id AND d2.keep_id = d.keep_id
    WHERE c.memory_id = b.memory_id
      AND s2.aspect = sv.aspect AND s2.scope_id = sv.scope_id
      AND c.ctid < b.ctid
  );
-- основания, не поместившиеся победителю (повтор пары) — уходит вместе с
-- проигравшими стансами
DELETE FROM stance_basis b
USING entity_dups d JOIN stances s ON s.entity_id = d.dup_id
WHERE b.stance_id = s.id
  AND s.id NOT IN (SELECT winner_id FROM stance_survivors);
DELETE FROM stances s
USING entity_dups d
WHERE s.entity_id = d.dup_id
  AND s.id NOT IN (SELECT winner_id FROM stance_survivors);
UPDATE stances s SET entity_id = d.keep_id
FROM entity_dups d WHERE s.entity_id = d.dup_id;
DROP TABLE stance_survivors;

-- Связи памяти и алиасы — на каноническую сущность; точные повторы
-- (одна и та же связь/алиас дважды) удаляются.
UPDATE memory_links l SET entity_id = d.keep_id FROM entity_dups d WHERE l.entity_id = d.dup_id;
DELETE FROM memory_links a USING memory_links b
WHERE a.ctid < b.ctid AND a.memory_id = b.memory_id AND a.entity_id = b.entity_id AND a.role = b.role;
UPDATE entity_aliases al SET entity_id = d.keep_id FROM entity_dups d WHERE al.entity_id = d.dup_id;
DELETE FROM entity_aliases a USING entity_aliases b
WHERE a.ctid < b.ctid AND a.entity_id = b.entity_id AND a.alias = b.alias AND a.scope_id = b.scope_id;

-- Сами дубликаты сущностей уходят: все зависимости уже на канонических строках.
DELETE FROM entities e USING entity_dups d WHERE e.id = d.dup_id;

DROP TABLE entity_dups;

DROP INDEX IF EXISTS entities_scope_name_idx; -- заменён уникальным по name_key
CREATE UNIQUE INDEX entities_scope_name_key_uidx ON entities (scope_id, name_key);
