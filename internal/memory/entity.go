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

// FindOrCreate находит сущность по имени (регистр нечувствителен) в скоупе
// или создаёт новую. Вызывается в транзакции записи.
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

	lower := strings.ToLower(trimmed)
	const cols = "id, kind, canonical_name, scope_id"
	var e Entity
	err := db.QueryRow(ctx,
		`SELECT `+cols+` FROM entities WHERE scope_id = $1 AND lower(canonical_name) = $2 LIMIT 1`,
		scopeID, lower).Scan(&e.ID, &e.Kind, &e.CanonicalName, &e.ScopeID)
	if err == nil {
		return e, nil
	}
	if !isNoRows(err) {
		return Entity{}, fmt.Errorf("entities: поиск по имени: %w", err)
	}

	err = db.QueryRow(ctx,
		`INSERT INTO entities (kind, canonical_name, scope_id) VALUES ($1, $2, $3)
		 RETURNING `+cols,
		kind, trimmed, scopeID).Scan(&e.ID, &e.Kind, &e.CanonicalName, &e.ScopeID)
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
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return nil, nil
	}

	rows, err := db.Query(ctx,
		`SELECT DISTINCT e.id, e.kind, e.canonical_name, e.scope_id
		 FROM entities e
		 LEFT JOIN entity_aliases a ON a.entity_id = e.id
		 WHERE ($1 = 'internal' OR e.scope_id IN ($1, 'public'))
		   AND (lower(e.canonical_name) = $2
		        OR (lower(a.alias) = $2 AND (a.scope_id = $1 OR a.scope_id = 'public')))
		 LIMIT 10`,
		audience, lower)
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
// (поведение TS-справочника); повтор той же пары — no-op.
func (EntityRepo) AddAlias(ctx context.Context, db DBTX, entityID int64, alias string) error {
	trimmed := strings.TrimSpace(alias)
	if trimmed == "" {
		return fmt.Errorf("%w: пустой алиас", ErrInvalidCandidate)
	}
	tag, err := db.Exec(ctx,
		`INSERT INTO entity_aliases (entity_id, alias, scope_id)
		 SELECT $1, $2, e.scope_id FROM entities e WHERE e.id = $1
		 AND NOT EXISTS (
		   SELECT 1 FROM entity_aliases a WHERE a.entity_id = $1 AND lower(a.alias) = lower($2)
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
