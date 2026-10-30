// Отношения с людьми (§7.3): числовые регуляторы отношения к человеку по чату.
// Familiarity растёт по runtime-правилу от значимых взаимодействий; affinity/trust
// меняются ограниченно и с основанием (основание — в событии deltas_applied
// журнала хода, §12.1). Не сбрасываются decay-ом настроения. Поведение
// перенесено из TS-справочника src/character/relationships.ts.
package character

import (
	"context"
	"fmt"
	"time"
)

// Relationship — регуляторы по человеку в scope чата (§7.3).
type Relationship struct {
	PersonEntityID int64
	ScopeID        string
	Familiarity    float64 // 0..1
	Affinity       float64 // -1..1
	Trust          float64 // 0..1
	UpdatedAt      time.Time
}

// RelationshipLimits — лимиты изменения за ход (§7.3).
var RelationshipLimits = struct {
	AffinityPerTurn               float64
	TrustPerTurn                  float64
	FamiliarityPerSignificantTurn float64
}{
	AffinityPerTurn:               0.1,
	TrustPerTurn:                  0.1,
	FamiliarityPerSignificantTurn: 0.05,
}

// defaultTrust — стартовое доверие незнакомцу.
const defaultTrust = 0.2

// RelationshipRepo — таблица relationships (миграция 000005).
type RelationshipRepo struct{}

// NewRelationshipRepo создаёт репозиторий (без состояния).
func NewRelationshipRepo() *RelationshipRepo { return &RelationshipRepo{} }

const relationshipColumns = `person_entity_id, scope_id, familiarity, affinity, trust, updated_at`

func scanRelationship(row interface{ Scan(...any) error }) (Relationship, error) {
	var r Relationship
	if err := row.Scan(&r.PersonEntityID, &r.ScopeID, &r.Familiarity, &r.Affinity, &r.Trust, &r.UpdatedAt); err != nil {
		return Relationship{}, err
	}
	return r, nil
}

// Read читает отношение; отсутствие записи — нейтральное стартовое
// (familiarity 0, affinity 0, trust = defaultTrust).
func (r *RelationshipRepo) Read(ctx context.Context, db DBTX, personEntityID int64, scopeID string) (Relationship, error) {
	rel, err := scanRelationship(db.QueryRow(ctx,
		`SELECT `+relationshipColumns+` FROM relationships WHERE person_entity_id = $1 AND scope_id = $2`,
		personEntityID, scopeID))
	if isNoRows(err) {
		return Relationship{
			PersonEntityID: personEntityID,
			ScopeID:        scopeID,
			Familiarity:    0,
			Affinity:       0,
			Trust:          defaultTrust,
			UpdatedAt:      time.Now(),
		}, nil
	}
	if err != nil {
		return Relationship{}, fmt.Errorf("relationship: чтение не удалось: %w", err)
	}
	return rel, nil
}

// Ensure создаёт строку, если ещё нет (внутри транзакции применения).
func (r *RelationshipRepo) Ensure(ctx context.Context, db DBTX, personEntityID int64, scopeID string) error {
	_, err := db.Exec(ctx,
		`INSERT INTO relationships (person_entity_id, scope_id, trust)
		 VALUES ($1, $2, $3) ON CONFLICT (person_entity_id, scope_id) DO NOTHING`,
		personEntityID, scopeID, defaultTrust)
	if err != nil {
		return fmt.Errorf("relationship: строка не создана: %w", err)
	}
	return nil
}

// ApplyDelta применяет ограниченную дельту affinity/trust за ход ВНУТРИ
// транзакции. Изменение сжимается до ±PerTurn, итог зажимается диапазоном.
func (r *RelationshipRepo) ApplyDelta(ctx context.Context, db DBTX, personEntityID int64, scopeID string, affinityDelta, trustDelta *float64) (Relationship, error) {
	if err := r.Ensure(ctx, db, personEntityID, scopeID); err != nil {
		return Relationship{}, err
	}
	// Блокировка строки: два хода к одному человеку не затирают друг друга.
	current, err := scanRelationship(db.QueryRow(ctx,
		`SELECT `+relationshipColumns+` FROM relationships
		 WHERE person_entity_id = $1 AND scope_id = $2 FOR UPDATE`,
		personEntityID, scopeID))
	if err != nil {
		return Relationship{}, fmt.Errorf("relationship: блокировка строки не удалась: %w", err)
	}
	nextAffinity := clamp(
		current.Affinity+clampDeltaPtr(affinityDelta, RelationshipLimits.AffinityPerTurn), -1, 1)
	nextTrust := clamp(
		current.Trust+clampDeltaPtr(trustDelta, RelationshipLimits.TrustPerTurn), 0, 1)
	rel, err := scanRelationship(db.QueryRow(ctx,
		`UPDATE relationships SET affinity = $3, trust = $4, updated_at = now()
		 WHERE person_entity_id = $1 AND scope_id = $2
		 RETURNING `+relationshipColumns,
		personEntityID, scopeID, nextAffinity, nextTrust))
	if err != nil {
		return Relationship{}, fmt.Errorf("relationship: дельта не применена: %w", err)
	}
	return rel, nil
}

// GrowFamiliarity — рост от значимого взаимодействия (runtime-правило, не модель).
func (r *RelationshipRepo) GrowFamiliarity(ctx context.Context, db DBTX, personEntityID int64, scopeID string, amount float64) (Relationship, error) {
	if err := r.Ensure(ctx, db, personEntityID, scopeID); err != nil {
		return Relationship{}, err
	}
	rel, err := scanRelationship(db.QueryRow(ctx,
		`UPDATE relationships
		 SET familiarity = LEAST(1, familiarity + $3), updated_at = now()
		 WHERE person_entity_id = $1 AND scope_id = $2
		 RETURNING `+relationshipColumns,
		personEntityID, scopeID, amount))
	if err != nil {
		return Relationship{}, fmt.Errorf("relationship: familiarity не выросла: %w", err)
	}
	return rel, nil
}

func clampDeltaPtr(v *float64, limit float64) float64 {
	if v == nil {
		return 0
	}
	return clamp(*v, -limit, limit)
}

// clampLimitPtr сжимает дельту до лимита для аудит-следа (событие deltas_applied
// заявляет только то, что реально применено, §7.3); не задана — nil (поле не
// менялось, в событии его нет).
func clampLimitPtr(v *float64, limit float64) *float64 {
	if v == nil {
		return nil
	}
	clamped := clamp(*v, -limit, limit)
	return &clamped
}

// DescribeRelationship — детерминированное описание для контекста (§7.3).
func DescribeRelationship(r Relationship) string {
	var parts []string
	switch {
	case r.Familiarity < 0.2:
		parts = append(parts, "почти незнаком")
	case r.Familiarity < 0.6:
		parts = append(parts, "уже неплохо знаком")
	default:
		parts = append(parts, "хорошо знаешь")
	}
	if r.Affinity > 0.3 {
		parts = append(parts, "он тебе симпатичен")
	} else if r.Affinity < -0.3 {
		parts = append(parts, "он тебе неприятен")
	}
	if r.Trust > 0.6 {
		parts = append(parts, "доверяешь ему")
	} else if r.Trust < 0.15 {
		parts = append(parts, "доверия мало")
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
