// Entities (спека §8.1): идентификаторы предметов памяти. Описания, спорные
// атрибуты и оценки живут записями memory_items, не полями карточки.
// Глобальной уникальности имени нет (§18): одно имя в двух чатах — две
// разные сущности, доступ не смешивается; неоднозначность не объединяется
// автоматически (§8.5).
package memory

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Виды сущностей (§8.1).
const (
	EntityPerson  = "person"
	EntityTopic   = "topic"
	EntityPlace   = "place"
	EntityObject  = "object"
	EntityEvent   = "event"
	EntityProject = "project"
)

var entityKinds = map[string]bool{
	EntityPerson: true, EntityTopic: true, EntityPlace: true,
	EntityObject: true, EntityEvent: true, EntityProject: true,
}

// IsValidEntityKind проверяет вид сущности.
func IsValidEntityKind(kind string) bool { return entityKinds[kind] }

// Entity — строка таблицы entities.
type Entity struct {
	ID            int64
	Kind          string
	CanonicalName string
	ScopeID       string
}

// EntityRepo — entities и entity_aliases.
type EntityRepo struct{}

// entityKeyExpr — SQL-выражение нормализованного ключа имени; зеркало
// generated-колонки entities.name_key (миграция 000011): приведение к NFC
// (легаси-строки могли лечь в NFD — macOS/Telegram отдают разложенную форму),
// сжатие пробельных серий в один пробел, обрезка краёв, lower, затем ё→е —
// именно в этом порядке: replace до lower() пропускал заглавную «Ё»
// (приёмка 17.12). Вычисляется на обеих сторонах сравнения — ключ запроса и
// хранилища всегда в одной нормализации, рассинхрона Go/SQL не бывает.
// NFC в самом выражении: сравнение идёт с ключом ХРАНИМОГО имени, нормализация
// параметра в коде этого не спасала. normalize() — PG13+, immutable.
const entityKeyExpr = `replace(lower(btrim(regexp_replace(normalize(%s, NFC), '\s+', ' ', 'g'))), 'ё', 'е')`

func entityKeySQL(column string) string { return fmt.Sprintf(entityKeyExpr, column) }

// DisplayName — имя сущности для человека и модели: последний алиас
// (отображаемое имя), иначе каноническое имя. Ключ `user:<id>` в панель и
// карточку не попадает (M7, замечание ревью).
func (EntityRepo) DisplayName(ctx context.Context, db DBTX, e Entity) (string, error) {
	var alias string
	err := db.QueryRow(ctx,
		`SELECT alias FROM entity_aliases WHERE entity_id = $1 ORDER BY id DESC LIMIT 1`,
		e.ID).Scan(&alias)
	if isNoRows(err) {
		return e.CanonicalName, nil
	}
	if err != nil {
		return "", fmt.Errorf("entities: отображаемое имя не читается: %w", err)
	}
	if trimmed := strings.TrimSpace(alias); trimmed != "" {
		return trimmed, nil
	}
	return e.CanonicalName, nil
}

// adoptLegacyPerson — совместимость с сущностями, созданными до M7: ключом
// было отображаемое имя. При первом обращении актора такая сущность
// «усыновляется» (ей добавляется алиас user:<id>), поэтому накопленные
// отношения и карточка не обнуляются. Уже занятую другим актором сущность
// (есть любой алиас user:*) не трогаем: два «Саши» должны разойтись.
func (EntityRepo) adoptLegacyPerson(ctx context.Context, db DBTX, actorID int64, displayName, scopeID string) (Entity, bool, error) {
	var e Entity
	err := db.QueryRow(ctx,
		`SELECT id, kind, canonical_name, scope_id FROM entities e
		 WHERE e.kind = $1 AND e.scope_id = $2 AND e.name_key = `+entityKeySQL("$3")+`
		 LIMIT 1`,
		EntityPerson, scopeID, norm.NFC.String(strings.TrimSpace(displayName))).
		Scan(&e.ID, &e.Kind, &e.CanonicalName, &e.ScopeID)
	if isNoRows(err) {
		return Entity{}, false, nil
	}
	if err != nil {
		return Entity{}, false, fmt.Errorf("entities: легаси-сущность не читается: %w", err)
	}
	if strings.HasPrefix(strings.ToLower(e.CanonicalName), "user:") {
		return Entity{}, false, nil // уже каноническая (возможно, наша)
	}
	var claimed bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM entity_aliases WHERE entity_id = $1 AND lower(alias) LIKE 'user:%')`,
		e.ID).Scan(&claimed); err != nil {
		return Entity{}, false, fmt.Errorf("entities: занятость легаси-сущности не читается: %w", err)
	}
	if claimed {
		return Entity{}, false, nil
	}
	if err := (EntityRepo{}).AddAlias(ctx, db, e.ID, PersonEntityName(actorID)); err != nil {
		return Entity{}, false, err
	}
	return e, true, nil
}

// PersonEntityName — каноническое имя сущности собеседника: actor_id
// стабилен, отображаемое имя меняется (M7). Отображаемое имя хранится
// алиасом, поэтому смена имени в Telegram не обнуляет отношения, а два
// «Саши» в группе не делят доверие.
func PersonEntityName(actorID int64) string {
	return fmt.Sprintf("user:%d", actorID)
}

// EnsurePerson находит/создаёт сущность собеседника по actor_id и добавляет
// отображаемое имя алиасом.
func (EntityRepo) EnsurePerson(ctx context.Context, db DBTX, actorID int64, displayName, scopeID string) (Entity, error) {
	display := strings.TrimSpace(displayName)
	// У актора уже есть каноническая сущность — не усыновляем чужую легаси-
	// запись с тем же отображаемым именем (замечание ревью): иначе смена имени
	// на «Колю» подменяла бы историю собеседника чужой карточкой.
	if existing, ok, err := (EntityRepo{}).findPersonByKey(ctx, db, PersonEntityName(actorID), scopeID); err != nil {
		return Entity{}, err
	} else if ok {
		if display != "" {
			if err := (EntityRepo{}).AddAlias(ctx, db, existing.ID, display); err != nil {
				return Entity{}, err
			}
		}
		return existing, nil
	}
	if display != "" {
		if legacy, ok, err := (EntityRepo{}).adoptLegacyPerson(ctx, db, actorID, display, scopeID); err != nil {
			return Entity{}, err
		} else if ok {
			// Старые отношения и карточка остаются на месте; отображаемое имя
			// уже каноническое — алиас добавляем для единообразия.
			if err := (EntityRepo{}).AddAlias(ctx, db, legacy.ID, display); err != nil {
				return Entity{}, err
			}
			return legacy, nil
		}
	}
	e, err := (EntityRepo{}).FindOrCreate(ctx, db, EntityPerson, PersonEntityName(actorID), scopeID)
	if err != nil {
		return Entity{}, err
	}
	if display != "" {
		if err := (EntityRepo{}).AddAlias(ctx, db, e.ID, display); err != nil {
			return Entity{}, err
		}
	}
	return e, nil
}

// findPersonByKey — поиск уже существующей person-сущности по точному ключу
// (canonical_name) без создания. Нужен, чтобы отличить «актора ещё нет» от
// «у актора уже есть своя карточка».
func (EntityRepo) findPersonByKey(ctx context.Context, db DBTX, key, scopeID string) (Entity, bool, error) {
	var e Entity
	err := db.QueryRow(ctx,
		`SELECT id, kind, canonical_name, scope_id FROM entities e
		 WHERE e.kind = $1 AND e.scope_id = $2
		   AND (e.name_key = `+entityKeySQL("$3")+`
		    OR e.id IN (
		      SELECT a.entity_id FROM entity_aliases a
		      WHERE a.scope_id = $2 AND `+entityKeySQL("a.alias")+` = `+entityKeySQL("$3")+`))
		 ORDER BY (e.name_key = `+entityKeySQL("$3")+`) DESC, e.id
		 LIMIT 1`,
		EntityPerson, scopeID, norm.NFC.String(strings.TrimSpace(key))).
		Scan(&e.ID, &e.Kind, &e.CanonicalName, &e.ScopeID)
	if isNoRows(err) {
		return Entity{}, false, nil
	}
	if err != nil {
		return Entity{}, false, fmt.Errorf("entities: сущность актора не читается: %w", err)
	}
	return e, true, nil
}

// FindOrCreate находит сущность по нормализованному имени или алиасу в скоупе
// записи или создаёт новую. Нормализация (17.12): регистр, ё/е, внутренние и
// хвостовые пробелы, NFC — «Алёна» и «АЛЕНА» одна сущность; сокращение
// («Данил» для «Данила») находит по алиасу. Вызывается в транзакции записи;
// уникальный индекс (scope_id, name_key) + ON CONFLICT сводят гонку двух
// ходов в одну строку.
func (EntityRepo) FindOrCreate(ctx context.Context, db DBTX, kind, name, scopeID string) (Entity, error) {
	if !entityKinds[kind] {
		return Entity{}, fmt.Errorf("%w: неизвестный вид сущности %q", ErrInvalidCandidate, kind)
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return Entity{}, fmt.Errorf("%w: пустое имя сущности", ErrInvalidCandidate)
	}
	if !IsValidScope(scopeID) {
		return Entity{}, fmt.Errorf("%w: %q", ErrInvalidScope, scopeID)
	}
	canonical := norm.NFC.String(trimmed)

	const cols = "id, kind, canonical_name, scope_id"
	var e Entity
	err := db.QueryRow(ctx,
		`SELECT `+cols+` FROM entities e
		 WHERE e.kind = $3
		   AND (e.scope_id = $1 AND e.name_key = `+entityKeySQL("$2")+`
		    OR e.id IN (
		      SELECT a.entity_id FROM entity_aliases a
		      WHERE a.scope_id = $1 AND `+entityKeySQL("a.alias")+` = `+entityKeySQL("$2")+`))
		 ORDER BY (e.name_key = `+entityKeySQL("$2")+`) DESC, e.id
		 LIMIT 1`,
		scopeID, canonical, kind).Scan(&e.ID, &e.Kind, &e.CanonicalName, &e.ScopeID)
	if err == nil {
		return e, nil
	}
	if !isNoRows(err) {
		return Entity{}, fmt.Errorf("entities: поиск по имени: %w", err)
	}

	// Гонка двух ходов: оба прошли поиск вхолостую и вставляют — конфликт
	// уникального индекса возвращает существующую строку (no-op DO UPDATE
	// нужен только чтобы RETURNING дал строку).
	err = db.QueryRow(ctx,
		`INSERT INTO entities (kind, canonical_name, scope_id) VALUES ($1, $2, $3)
		 ON CONFLICT (scope_id, name_key) DO UPDATE SET scope_id = entities.scope_id
		 RETURNING `+cols,
		kind, canonical, scopeID).Scan(&e.ID, &e.Kind, &e.CanonicalName, &e.ScopeID)
	if err != nil {
		return Entity{}, fmt.Errorf("entities: вставка: %w", err)
	}
	return e, nil
}

// ResolveByName ищет сущность по каноническому имени или алиасу для конкретной
// аудитории (§8.1, §9.2). Приватная сущность не раскрывается через поиск
// названий из чужого чата; алиас тоже имеет свою область доступа — алиас
// чужого скоупа не открывает даже публичную сущность. Невидимые сущности
// не отличимы от несуществующих.
func (EntityRepo) ResolveByName(ctx context.Context, db DBTX, name, audience string) ([]Entity, error) {
	if !IsValidScope(audience) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	key := norm.NFC.String(strings.TrimSpace(name))
	if key == "" {
		return nil, nil
	}

	rows, err := db.Query(ctx,
		`SELECT DISTINCT e.id, e.kind, e.canonical_name, e.scope_id
		 FROM entities e
		 LEFT JOIN entity_aliases a ON a.entity_id = e.id
		 WHERE ($1 = 'internal' OR e.scope_id IN ($1, 'public'))
		   AND (e.name_key = `+entityKeySQL("$2")+`
		        OR (`+entityKeySQL("a.alias")+` = `+entityKeySQL("$2")+` AND (a.scope_id = $1 OR a.scope_id = 'public')))
		 LIMIT 10`,
		audience, key)
	if err != nil {
		return nil, fmt.Errorf("entities: поиск по имени/алиасу: %w", err)
	}
	defer rows.Close()

	var out []Entity
	for rows.Next() {
		var e Entity
		if err := rows.Scan(&e.ID, &e.Kind, &e.CanonicalName, &e.ScopeID); err != nil {
			return nil, fmt.Errorf("entities: чтение строки поиска: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get возвращает сущность по id, если она видна аудитории; иначе ErrNotFound
// (существование невидимой сущности не раскрывается — §9.2).
func (EntityRepo) Get(ctx context.Context, db DBTX, id int64, audience string) (Entity, error) {
	if !IsValidScope(audience) {
		return Entity{}, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	var e Entity
	err := db.QueryRow(ctx,
		`SELECT id, kind, canonical_name, scope_id FROM entities
		 WHERE id = $1 AND ($2 = 'internal' OR scope_id IN ($2, 'public'))`,
		id, audience).Scan(&e.ID, &e.Kind, &e.CanonicalName, &e.ScopeID)
	if isNoRows(err) {
		return Entity{}, ErrNotFound
	}
	if err != nil {
		return Entity{}, fmt.Errorf("entities: чтение по id: %w", err)
	}
	return e, nil
}

// AddAlias добавляет алиас сущности. Алиас наследует скоуп сущности
// (поведение TS-справочника); повтор той же пары — no-op (сравнение в
// нормализации ключа имени, 17.12: «Даня» и «ДАНЯ » — один алиас).
func (EntityRepo) AddAlias(ctx context.Context, db DBTX, entityID int64, alias string) error {
	trimmed := norm.NFC.String(strings.TrimSpace(alias))
	if trimmed == "" {
		return fmt.Errorf("%w: пустой алиас", ErrInvalidCandidate)
	}
	tag, err := db.Exec(ctx,
		`INSERT INTO entity_aliases (entity_id, alias, scope_id)
		 SELECT $1, $2, e.scope_id FROM entities e WHERE e.id = $1
		 AND NOT EXISTS (
		   SELECT 1 FROM entity_aliases a
		   WHERE a.entity_id = $1 AND `+entityKeySQL("a.alias")+` = `+entityKeySQL("$2")+`
		 )`,
		entityID, trimmed)
	if err != nil {
		return fmt.Errorf("entity_aliases: вставка: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Сущности нет (SELECT не дал строк) либо алиас уже был.
		var exists bool
		if err := db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM entities WHERE id = $1)", entityID).Scan(&exists); err != nil {
			return fmt.Errorf("entity_aliases: проверка сущности: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}
