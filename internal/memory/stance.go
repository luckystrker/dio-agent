// Stances (спека §8.4): компактное текущее отношение к сущности по аспекту.
// Противоречивые впечатления допустимы — несколько аспектов. Изменение
// сохраняется с основаниями и revision; предыдущие основания не стираются
// (stance_basis только дополняется). Истина не выражается: strength —
// устойчивость позиции, interest — желание возвращаться к теме.
package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Stance — строка stances.
type Stance struct {
	ID        int64
	EntityID  int64
	Aspect    string
	ScopeID   string
	Summary   string
	Strength  float64
	Interest  float64
	Revision  int
	UpdatedAt time.Time
}

// StanceInput — пересмотр stance от имени runtime: одно значение на пару
// (entity, aspect, scope); ревизия и обновление оснований — внутри.
type StanceInput struct {
	EntityID       int64
	Aspect         string
	ScopeID        string
	Summary        string
	Strength       float64
	Interest       float64
	BasisMemoryIDs []int64 // добавляются к существующим основаниям, не заменяют их
}

// Validate проверяет пересмотр (§10.1: runtime проверяет значения и ссылки).
func (s StanceInput) Validate() error {
	if s.EntityID <= 0 {
		return fmt.Errorf("%w: не указана сущность stance", ErrInvalidCandidate)
	}
	if strings.TrimSpace(s.Aspect) == "" {
		return fmt.Errorf("%w: пустой аспект stance", ErrInvalidCandidate)
	}
	if strings.TrimSpace(s.Summary) == "" {
		return fmt.Errorf("%w: пустое резюме stance", ErrInvalidCandidate)
	}
	if !IsValidScope(s.ScopeID) {
		return fmt.Errorf("%w: %q", ErrInvalidScope, s.ScopeID)
	}
	if s.Strength < 0 || s.Strength > 1 {
		return fmt.Errorf("%w: strength %v вне 0..1", ErrInvalidCandidate, s.Strength)
	}
	if s.Interest < 0 || s.Interest > 1 {
		return fmt.Errorf("%w: interest %v вне 0..1", ErrInvalidCandidate, s.Interest)
	}
	return nil
}

// Stances — репозиторий stances (+ stance_basis).
type Stances struct{}

const stanceCols = "id, entity_id, aspect, scope_id, summary, strength, interest, revision, updated_at"

func scanStance(row pgxRow) (Stance, error) {
	var s Stance
	err := row.Scan(&s.ID, &s.EntityID, &s.Aspect, &s.ScopeID, &s.Summary,
		&s.Strength, &s.Interest, &s.Revision, &s.UpdatedAt)
	if err != nil {
		return Stance{}, err
	}
	return s, nil
}

// Upsert записывает текущее отношение (§8.4): при повторе пары
// (entity, aspect, scope) summary/strength/interest обновляются, revision
// растёт, основания только добавляются. Неизвестная ссылка на память
// отклоняется FK (§10.1).
func (Stances) Upsert(ctx context.Context, db DBTX, in StanceInput) (Stance, error) {
	if err := in.Validate(); err != nil {
		return Stance{}, err
	}

	var st Stance
	err := db.QueryRow(ctx,
		`INSERT INTO stances (entity_id, aspect, scope_id, summary, strength, interest)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (entity_id, aspect, scope_id)
		 DO UPDATE SET summary = EXCLUDED.summary,
		              strength = EXCLUDED.strength,
		              interest = EXCLUDED.interest,
		              revision = stances.revision + 1,
		              updated_at = now()
		 RETURNING `+stanceCols,
		in.EntityID, in.Aspect, in.ScopeID, in.Summary, in.Strength, in.Interest).
		Scan(&st.ID, &st.EntityID, &st.Aspect, &st.ScopeID, &st.Summary,
			&st.Strength, &st.Interest, &st.Revision, &st.UpdatedAt)
	if err != nil {
		return Stance{}, fmt.Errorf("stances: upsert: %w", err)
	}

	for _, memoryID := range in.BasisMemoryIDs {
		if _, err := db.Exec(ctx,
			`INSERT INTO stance_basis (stance_id, memory_id)
			 SELECT $1, $2 WHERE NOT EXISTS (
			   SELECT 1 FROM stance_basis WHERE stance_id = $1 AND memory_id = $2)`,
			st.ID, memoryID); err != nil {
			return Stance{}, fmt.Errorf("stance_basis: связывание: %w", err)
		}
	}
	return st, nil
}

// ForEntities — актуальные отношения по сущностям для аудитории (§10.4 п.3):
// видны stances своего скоупа и public; приватный stance на публичную
// сущность чужому чату не виден.
func (Stances) ForEntities(ctx context.Context, db DBTX, audience string, entityIDs []int64) ([]Stance, error) {
	if !IsValidScope(audience) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	if len(entityIDs) == 0 {
		return nil, nil
	}

	rows, err := db.Query(ctx,
		`SELECT `+stanceCols+` FROM stances
		 WHERE ($1 = 'internal' OR scope_id IN ($1, 'public')) AND entity_id = ANY($2)
		 ORDER BY updated_at DESC, id DESC`,
		audience, entityIDs)
	if err != nil {
		return nil, fmt.Errorf("stances: выборка по сущностям: %w", err)
	}
	defer rows.Close()

	var out []Stance
	for rows.Next() {
		st, err := scanStance(rows)
		if err != nil {
			return nil, fmt.Errorf("stances: чтение строки: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Basis — записи-основания stance для аудитории (§8.4: «при прямом вопросе
// "что думаешь про X" приоритет имеют stance по X и его основания»). Все
// основания сущуют в хранилище, аудитории выдаются только видимые (§9.2:
// карточка собирается сразу для конкретной аудитории); скрытые считаются
// отдельно, чтобы ответ мог честно сказать «часть оснований недоступна».
func (Stances) Basis(ctx context.Context, db DBTX, items Items, audience string, stanceID int64) (visible []Item, hidden int, err error) {
	if !IsValidScope(audience) {
		return nil, 0, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}

	rows, err := db.Query(ctx,
		`SELECT `+prefixedItemCols("m")+` FROM memory_items m
		 JOIN stance_basis b ON b.memory_id = m.id
		 LEFT JOIN provenance p ON p.id = m.provenance_id
		 WHERE b.stance_id = $1 AND `+visCond("m", 2)+`
		 ORDER BY m.salience DESC, m.recorded_at DESC, m.id DESC`,
		stanceID, audience)
	if err != nil {
		return nil, 0, fmt.Errorf("stance_basis: выборка оснований: %w", err)
	}
	defer rows.Close()
	visible, err = collectItems(rows)
	if err != nil {
		return nil, 0, err
	}

	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM stance_basis b
		 JOIN memory_items m ON m.id = b.memory_id
		 LEFT JOIN provenance p ON p.id = m.provenance_id
		 WHERE b.stance_id = $1 AND NOT (`+visCond("m", 2)+`)`,
		stanceID, audience).Scan(&hidden); err != nil {
		return nil, 0, fmt.Errorf("stance_basis: подсчёт скрытых оснований: %w", err)
	}
	return visible, hidden, nil
}
