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
	"time"

	"github.com/jackc/pgx/v5"
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
	pool *Pool
}

// NewBudgetStore создаёт хранилище поверх пула.
func NewBudgetStore(pool *Pool) *BudgetStore {
	return &BudgetStore{pool: pool}
}

// Reserve резервирует count модельных вызовов пула на сегодня под лимитом limit.
// limit <= 0 — без ограничения. Возвращает true, когда вызов разрешён: резерв
// создан в пределах лимита или операция уже оплачена (retry той же операции —
// §16). operationID должен быть детерминированным на попытку.
//
// Атомарность (блок 17.2): дубль operation_id отсекается INSERT ... ON CONFLICT
// DO NOTHING (конкурентный retry ждёт первого коммита и видит RowsAffected=0 —
// «уже оплачено», без ошибки 23505); счётчик дня читается под блокировкой строки
// FOR UPDATE в той же транзакции — параллельные резервы (прямой ход + фон)
// выстраиваются и сверх лимита не проходят. Превышение лимита откатывает и
// только что вставленный резерв.
func (s *BudgetStore) Reserve(ctx context.Context, operationID string, attempt int, pool string, count, limit int64) (bool, error) {
	if count <= 0 {
		count = 1
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("бюджет: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Reserve — в той же транзакции с check (§16). Уже оплачено? Конкурентный
	// retry того же operation_id упирается в уникальность и получает
	// RowsAffected=0 — лимит повторно не расходуется.
	reservedJSON, err := json.Marshal(map[string]int64{"calls": count})
	if err != nil {
		return false, fmt.Errorf("бюджет: резерв не сериализуется: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO budget_reservations (operation_id, attempt, pool, reserved, period)
		 VALUES ($1, $2, $3, $4::jsonb, CURRENT_DATE)
		 ON CONFLICT (operation_id) DO NOTHING`,
		operationID, attempt, pool, string(reservedJSON))
	if err != nil {
		return false, fmt.Errorf("бюджет: резерв не создан: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Уже оплачено/зарезервировано: retry операции лимит не расходует.
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("бюджет: резерв не зафиксирован: %w", err)
		}
		return true, nil
	}

	// Check под блокировкой строки счётчика: гонка с параллельным резервом
	// видит закоммиченное значение, а не одновременное чтение одного нуля.
	if _, err := tx.Exec(ctx,
		`INSERT INTO budget_counters (pool, period, reserved_calls, updated_at)
		 VALUES ($1, CURRENT_DATE, 0, now())
		 ON CONFLICT (pool, period) DO NOTHING`, pool); err != nil {
		return false, fmt.Errorf("бюджет: счётчик не создан: %w", err)
	}
	var reserved int64
	err = tx.QueryRow(ctx,
		`SELECT reserved_calls FROM budget_counters WHERE pool = $1 AND period = CURRENT_DATE FOR UPDATE`,
		pool).Scan(&reserved)
	if err != nil {
		return false, fmt.Errorf("бюджет: счётчик не читается: %w", err)
	}
	if limit > 0 && reserved+count > limit {
		return false, fmt.Errorf("%w: пул %s: зарезервировано %d, запрошено ещё %d при лимите %d",
			ErrBudgetExhausted, pool, reserved, count, limit)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE budget_counters SET reserved_calls = reserved_calls + $2, updated_at = now()
		 WHERE pool = $1 AND period = CURRENT_DATE`, pool, count); err != nil {
		return false, fmt.Errorf("бюджет: счётчик не увеличен: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("бюджет: резерв не зафиксирован: %w", err)
	}
	return true, nil
}

// Release возвращает неиспользованный резерв (§16, блок 17.3): вызов модели не
// состоялся (отмена хода, сбой до фактического запроса) — счётчик дня не
// выжигается храповиком. Идемпотентен: повторный вызов и уже оплаченная
// операция (actual записан) — честный no-op. Счётчик своего дня опускается
// не ниже нуля.
func (s *BudgetStore) Release(ctx context.Context, operationID, pool string, count int64) error {
	if count <= 0 {
		count = 1
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("бюджет: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var period time.Time
	err = tx.QueryRow(ctx,
		`DELETE FROM budget_reservations WHERE operation_id = $1 AND actual IS NULL RETURNING period`,
		operationID).Scan(&period)
	if errors.Is(err, pgx.ErrNoRows) {
		// Возвращать нечего: резерв уже оплачен или уже возвращён.
		return tx.Commit(ctx)
	}
	if err != nil {
		return fmt.Errorf("бюджет: резерв не возвращается: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE budget_counters
		 SET reserved_calls = GREATEST(0, reserved_calls - $2), updated_at = now()
		 WHERE pool = $1 AND period = $3`, pool, count, period); err != nil {
		return fmt.Errorf("бюджет: счётчик не возвращён: %w", err)
	}
	return tx.Commit(ctx)
}

// ActualUsage — фактическая стоимость операции. CachedTokens — токены
// префикса, прочитанные из кэша провайдера (nil — провайдер их не сообщил):
// наблюдаемость попадания в префикс (решение 2026-11-02).
type ActualUsage struct {
	Tokens       int64
	Calls        int64
	CachedTokens *int64
	Unknown      bool
}

// RecordActual фиксирует фактический usage операции (§16: отчётность различает
// reserved и spent). unknownUsage=true — стоимость неизвестна (timeout): actual
// не пишется нулевым, только счётчики вызовов.
func (s *BudgetStore) RecordActual(ctx context.Context, operationID, pool string, tokens, calls int64, unknownUsage bool) error {
	return s.RecordActualUsage(ctx, operationID, pool, ActualUsage{Tokens: tokens, Calls: calls, Unknown: unknownUsage})
}

// RecordActualUsage — RecordActual с отдельным полем кэшированных токенов.
// Кэш-попадание не меняет сумму tokens (провайдер уже учёл его в prompt_tokens),
// но видно в actual как cached_tokens: по нему проверяется, что префикс не
// пересобирается между ходами.
func (s *BudgetStore) RecordActualUsage(ctx context.Context, operationID, pool string, usage ActualUsage) error {
	actual := map[string]any{"tokens": usage.Tokens, "calls": usage.Calls}
	if usage.CachedTokens != nil {
		actual["cached_tokens"] = *usage.CachedTokens
	}
	if usage.Unknown {
		// Неизвестная стоимость ≠ нулевая: actual не заполняется токенами.
		actual = map[string]any{"calls": usage.Calls, "unknown": true}
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
		      updated_at = now()`, pool, usage.Tokens, usage.Calls)
	if err != nil {
		return fmt.Errorf("бюджет: фактический счётчик не увеличен: %w", err)
	}
	return nil
}

// Available — peek без резерва: остался ли в пуле запас на count вызовов
// сегодня (limit <= 0 — не ограничивать). Резерв не создаётся: фон проверяет
// это до начала работы, чтобы отложиться до нового дня, не расходуя потолок
// попыток и не плодя шаги WAL (находка 2026-10-31: собственный дневной лимит
// приравнивался к вечному сбою провайдера и уводил задачу в cooldown 24 ч).
func (s *BudgetStore) Available(ctx context.Context, pool string, count, limit int64) (bool, error) {
	if limit <= 0 {
		return true, nil
	}
	if count <= 0 {
		count = 1
	}
	var reserved int64
	err := s.pool.QueryRow(ctx,
		`SELECT reserved_calls FROM budget_counters WHERE pool = $1 AND period = CURRENT_DATE`,
		pool).Scan(&reserved)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("бюджет: счётчик дня не читается: %w", err)
	}
	return reserved+count <= limit, nil
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
