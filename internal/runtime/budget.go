// Бюджеты модельных вызовов (§16, таблицы §18 budget_reservations/budget_counters).
// Один механизм обслуживает conversation, background и maintenance (пул задаёт
// вызывающий); общий дневной потолок применяется поверх пулов.
//
// До операции атомарно резервируется оценка верхней стоимости: check и reserve
// — одна транзакция (§16); middleware хода не пропускает модельный вызов без
// резерва. У reservation уникальный operation_id: retry той же операции не
// резервирует повторно, новая платная попытка получает отдельный attempt ID.
// После операции фиксируется фактический usage; неизвестная после timeout
// стоимость не считается нулевой (actual остаётся NULL).
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrBudgetExhausted — дневной лимит пула исчерпан: операция не выполняется.
var ErrBudgetExhausted = errors.New("бюджет: дневной лимит пула исчерпан (§16)")

// Пулы бюджета (§16).
const (
	BudgetPoolConversation = "conversation"
	BudgetPoolBackground   = "background"
	BudgetPoolMaintenance  = "maintenance"
)

// BudgetStore — резервирование бюджета в PostgreSQL.
type BudgetStore struct {
	pool *pgxpool.Pool
}

// NewBudgetStore создаёт хранилище поверх пула.
func NewBudgetStore(pool *pgxpool.Pool) *BudgetStore {
	return &BudgetStore{pool: pool}
}

// Reserve резервирует count модельных вызовов пула на сегодня под лимитом limit.
// limit <= 0 — без ограничения. Возвращает true, когда вызов разрешён: резерв
// создан в пределах лимита или операция уже оплачена (retry той же операции —
// §16). operationID должен быть детерминированным на попытку.
func (s *BudgetStore) Reserve(ctx context.Context, operationID string, attempt int, pool string, count, limit int64) (bool, error) {
	if count <= 0 {
		count = 1
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("бюджет: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Уже оплачено? retry операции лимит не расходует.
	var already bool
	err = tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM budget_reservations WHERE operation_id = $1)`,
		operationID).Scan(&already)
	if err != nil {
		return false, fmt.Errorf("бюджет: резерв не проверяется: %w", err)
	}
	if already {
		return true, nil
	}

	// Check: сколько вызовов пула зарезервировано сегодня.
	var reserved int64
	err = tx.QueryRow(ctx,
		`SELECT reserved_calls FROM budget_counters WHERE pool = $1 AND period = CURRENT_DATE`,
		pool).Scan(&reserved)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("бюджет: счётчик не читается: %w", err)
	}
	if limit > 0 && reserved+count > limit {
		return false, fmt.Errorf("%w: пул %s: зарезервировано %d, запрошено ещё %d при лимите %d",
			ErrBudgetExhausted, pool, reserved, count, limit)
	}

	// Reserve — в той же транзакции с check (§16).
	reservedJSON, err := json.Marshal(map[string]int64{"calls": count})
	if err != nil {
		return false, fmt.Errorf("бюджет: резерв не сериализуется: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO budget_reservations (operation_id, attempt, pool, reserved, period)
		 VALUES ($1, $2, $3, $4::jsonb, CURRENT_DATE)`,
		operationID, attempt, pool, string(reservedJSON)); err != nil {
		return false, fmt.Errorf("бюджет: резерв не создан: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO budget_counters (pool, period, reserved_calls, updated_at)
		 VALUES ($1, CURRENT_DATE, $2, now())
		 ON CONFLICT (pool, period) DO UPDATE
		   SET reserved_calls = budget_counters.reserved_calls + $2, updated_at = now()`,
		pool, count); err != nil {
		return false, fmt.Errorf("бюджет: счётчик не увеличен: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("бюджет: резерв не зафиксирован: %w", err)
	}
	return true, nil
}

// RecordActual фиксирует фактический usage операции (§16: отчётность различает
// reserved и spent). unknownUsage=true — стоимость неизвестна (timeout): actual
// не пишется нулевым, только счётчики вызовов.
func (s *BudgetStore) RecordActual(ctx context.Context, operationID, pool string, tokens, calls int64, unknownUsage bool) error {
	actual := map[string]any{"tokens": tokens, "calls": calls}
	if unknownUsage {
		// Неизвестная стоимость ≠ нулевая: actual не заполняется токенами.
		actual = map[string]any{"calls": calls, "unknown": true}
	}
	raw, err := json.Marshal(actual)
	if err != nil {
		return fmt.Errorf("бюджет: actual не сериализуется: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE budget_reservations SET actual = $2::jsonb WHERE operation_id = $1`,
		operationID, string(raw)); err != nil {
		return fmt.Errorf("бюджет: actual не записан: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO budget_counters (pool, period, actual_tokens, actual_calls, updated_at)
		VALUES ($1, CURRENT_DATE, $2, $3, now())
		ON CONFLICT (pool, period) DO UPDATE
		  SET actual_tokens = budget_counters.actual_tokens + $2,
		      actual_calls  = budget_counters.actual_calls + $3,
		      updated_at = now()`, pool, tokens, calls)
	if err != nil {
		return fmt.Errorf("бюджет: фактический счётчик не увеличен: %w", err)
	}
	return nil
}

// ReservedToday — сколько операций пула зарезервировано сегодня (диагностика §20).
func (s *BudgetStore) ReservedToday(ctx context.Context, pool string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM budget_reservations
		WHERE pool = $1 AND period = CURRENT_DATE`, pool).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("бюджет: счётчик дня не читается: %w", err)
	}
	return n, nil
}
