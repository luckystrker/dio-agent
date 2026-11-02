//go:build db

// БД-тесты атомарности Reserve (§16, блок 17.2): конкурентные резервы одного
// дня не превышают лимит (гонка счётчика), конкурентный дубль operation_id —
// ветка «уже оплачено», а не ошибка резерва (23505 глушила ход). Запуск:
//
//	go test -tags db ./internal/runtime/
//
// против Postgres из docker-compose; TEST_DATABASE_URL переопределяет DSN.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// reservedCallsToday — счётчик пула на сегодня (прямой SQL, мимо Store).
func reservedCallsToday(t *testing.T, pool *Pool, poolName string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT reserved_calls FROM budget_counters WHERE pool = $1 AND period = CURRENT_DATE`,
		poolName).Scan(&n); err != nil {
		t.Fatalf("счётчик пула %s не читается: %v", poolName, err)
	}
	return n
}

// TestReserveConcurrentRespectsLimit — параллельные Reserve (прямой ход + фон)
// читают один счётчик: разрешённых должно быть ровно столько, сколько влезает
// в лимит, счётчик не расходится с числом резервов.
func TestReserveConcurrentRespectsLimit(t *testing.T) {
	pool := newTestPool(t)
	store := NewBudgetStore(pool)
	ctx := context.Background()

	const (
		workers = 8
		limit   = 3
	)
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		allowed   int
		exhausted int
		failed    []error
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ok, err := store.Reserve(ctx,
				fmt.Sprintf("model:turn:1720:%d:0", i), 0, BudgetPoolConversation, 1, limit)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && ok:
				allowed++
			case errors.Is(err, ErrBudgetExhausted):
				exhausted++
			default:
				failed = append(failed, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(failed) > 0 {
		t.Fatalf("конкурентные резервы не должны падать ошибками БД: %v", failed)
	}
	if allowed != limit {
		t.Fatalf("при лимите %d разрешено %d вызовов (отклонено %d) — проверка и резерв не атомарны (§16)", limit, allowed, exhausted)
	}
	if got := reservedCallsToday(t, pool, BudgetPoolConversation); got != limit {
		t.Fatalf("счётчик дня %d, хочу %d — гонка счётчика расходится с лимитом", got, limit)
	}
	var rows int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM budget_reservations WHERE pool = $1 AND period = CURRENT_DATE`,
		BudgetPoolConversation).Scan(&rows); err != nil {
		t.Fatalf("резервы не читаются: %v", err)
	}
	if rows != limit {
		t.Fatalf("резервов в базе %d, хочу %d — сверх лимита просочились строки", rows, limit)
	}
}

// TestReserveConcurrentDuplicateOperation — два параллельных Reserve одного
// operation_id (retry, гонка прямого хода и восстановления): оба получают
// «разрешено», счётчик инкрементируется один раз, строка резерва одна.
// Внешняя транзакция держит строку счётчика, пока оба Reserve висят внутри, —
// так INSERT одного operation_id гарантированно пересекается (без этого
// последовательный прогон проходит и на старом коде через SELECT already-paid).
func TestReserveConcurrentDuplicateOperation(t *testing.T) {
	pool := newTestPool(t)
	store := NewBudgetStore(pool)
	ctx := context.Background()
	const op = "model:turn:1721:5:0:0"

	// Блокиратор: создаёт и держит строку счётчика дня незакоммиченной.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("блокиратор не открыт: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx,
		`INSERT INTO budget_counters (pool, period, reserved_calls, updated_at)
		 VALUES ($1, CURRENT_DATE, 0, now()) ON CONFLICT (pool, period) DO NOTHING`,
		BudgetPoolBackground); err != nil {
		t.Fatalf("строка счётчика не создана: %v", err)
	}
	if _, err := blocker.Exec(ctx,
		`SELECT reserved_calls FROM budget_counters WHERE pool = $1 AND period = CURRENT_DATE FOR UPDATE`,
		BudgetPoolBackground); err != nil {
		t.Fatalf("строка счётчика не заблокирована: %v", err)
	}

	var (
		wg         sync.WaitGroup
		results    [2]bool
		errs       [2]error
		startAgain = make(chan struct{})
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-startAgain
			results[i], errs[i] = store.Reserve(ctx, op, 0, BudgetPoolBackground, 1, 1)
		}(i)
	}
	close(startAgain)
	time.Sleep(200 * time.Millisecond) // оба Reserve дошли до счётчика и висят
	_ = blocker.Rollback(ctx)          // отпустить счётчик — оба Reserve завершаются
	wg.Wait()

	for i := 0; i < 2; i++ {
		if errs[i] != nil {
			t.Fatalf("резерв %d упал ошибкой вместо ветки «уже оплачено» (§16): %v", i, errs[i])
		}
		if !results[i] {
			t.Fatalf("резерв %d отклонён без ошибки — конкурентный дубль обязан проходить как retry", i)
		}
	}
	if got := reservedCallsToday(t, pool, BudgetPoolBackground); got != 1 {
		t.Fatalf("счётчик дня %d, хочу 1 — дубль operation_id инкрементировал счётчик", got)
	}
	var rows int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM budget_reservations WHERE operation_id = $1`, op).Scan(&rows); err != nil {
		t.Fatalf("резервы не читаются: %v", err)
	}
	if rows != 1 {
		t.Fatalf("строк резерва %d, хочу 1 — дубль operation_id создал вторую", rows)
	}
}

// TestReleaseReturnsReservationAndIdempotent — Release возвращает резерв в
// лимит дня (освободившееся место снова резервируется) и идемпотентен:
// повторный вызов — no-op без ухода счётчика в минус.
func TestReleaseReturnsReservationAndIdempotent(t *testing.T) {
	pool := newTestPool(t)
	store := NewBudgetStore(pool)
	ctx := context.Background()
	const (
		op    = "model:turn:1733:1:0:0"
		other = "model:turn:1733:1:0:1"
		limit = 1
	)

	if ok, err := store.Reserve(ctx, op, 0, BudgetPoolConversation, 1, limit); err != nil || !ok {
		t.Fatalf("первый резерв не прошёл: ok=%v err=%v", ok, err)
	}
	if got := reservedCallsToday(t, pool, BudgetPoolConversation); got != 1 {
		t.Fatalf("счётчик после резерва %d, хочу 1", got)
	}

	// Лимит занят: соседняя операция не проходит.
	if _, err := store.Reserve(ctx, other, 0, BudgetPoolConversation, 1, limit); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("занятый лимит должен отклонять: %v", err)
	}

	if err := store.Release(ctx, op, BudgetPoolConversation, 1); err != nil {
		t.Fatalf("Release не удался: %v", err)
	}
	if got := reservedCallsToday(t, pool, BudgetPoolConversation); got != 0 {
		t.Fatalf("счётчик после возврата %d, хочу 0 — день не освобождён", got)
	}
	var rows int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM budget_reservations WHERE operation_id = $1`, op).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("резерв не удалён: rows=%d err=%v", rows, err)
	}

	// Освободившееся место снова резервируется.
	if ok, err := store.Reserve(ctx, other, 0, BudgetPoolConversation, 1, limit); err != nil || !ok {
		t.Fatalf("резерв после возврата не прошёл: ok=%v err=%v", ok, err)
	}

	// Идемпотентность: Release несуществующего/уже возвращённого — no-op.
	if err := store.Release(ctx, op, BudgetPoolConversation, 1); err != nil {
		t.Fatalf("повторный Release не должен падать: %v", err)
	}
	if got := reservedCallsToday(t, pool, BudgetPoolConversation); got != 1 {
		t.Fatalf("повторный Release сдвинул счётчик: %d, хочу 1", got)
	}
}

// TestReleaseKeepsPaidReservation — оплаченная операция (actual записан)
// не возвращается: стоимость потрачена, счётчик не корректируется вниз.
func TestReleaseKeepsPaidReservation(t *testing.T) {
	pool := newTestPool(t)
	store := NewBudgetStore(pool)
	ctx := context.Background()
	const op = "model:turn:1734:1:0:0"

	if ok, err := store.Reserve(ctx, op, 0, BudgetPoolBackground, 1, 0); err != nil || !ok {
		t.Fatalf("резерв не прошёл: ok=%v err=%v", ok, err)
	}
	if err := store.RecordActual(ctx, op, BudgetPoolBackground, 120, 1, false); err != nil {
		t.Fatalf("actual не записан: %v", err)
	}
	if err := store.Release(ctx, op, BudgetPoolBackground, 1); err != nil {
		t.Fatalf("Release оплаченного не должен падать: %v", err)
	}
	var rows int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM budget_reservations WHERE operation_id = $1`, op).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("оплаченный резерв не должен удаляться: rows=%d err=%v", rows, err)
	}
	if got := reservedCallsToday(t, pool, BudgetPoolBackground); got != 1 {
		t.Fatalf("счётчик оплаченного дня сдвинут: %d, хочу 1", got)
	}
}

// TestReserveDuplicateAfterCommitAlreadyPaid — вторая половина ТЗ 17.2
// (конкурентную закрывает TestReserveConcurrentDuplicateOperation): повторный
// Reserve того же operation_id ПОСЛЕ коммита — «уже оплачено» без второго
// инкремента, и retry проходит даже когда лимит дня уже исчерпан другой
// операцией (§16: retry той же операции лимит не расходует).
func TestReserveDuplicateAfterCommitAlreadyPaid(t *testing.T) {
	pool := newTestPool(t)
	store := NewBudgetStore(pool)
	ctx := context.Background()
	const (
		op    = "model:turn:1735:3:0:0"
		other = "model:turn:1735:3:0:1"
		limit = 1
	)

	if ok, err := store.Reserve(ctx, op, 0, BudgetPoolConversation, 1, limit); err != nil || !ok {
		t.Fatalf("первый резерв не прошёл: ok=%v err=%v", ok, err)
	}
	// Лимит занят целиком: соседняя операция отклоняется.
	if _, err := store.Reserve(ctx, other, 0, BudgetPoolConversation, 1, limit); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("занятый лимит должен отклонять соседнюю операцию: %v", err)
	}
	// Retry той же операции после коммита: разрешено, счётчик не двигается.
	ok, err := store.Reserve(ctx, op, 0, BudgetPoolConversation, 1, limit)
	if err != nil || !ok {
		t.Fatalf("повторный резерв обязан проходить как «уже оплачено» даже при исчерпанном лимите: ok=%v err=%v", ok, err)
	}
	if got := reservedCallsToday(t, pool, BudgetPoolConversation); got != limit {
		t.Fatalf("счётчик дня %d, хочу %d — повторный Reserve инкрементировал счётчик", got, limit)
	}
	var rows int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM budget_reservations WHERE operation_id = $1`, op).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("строк резерва %d (err=%v), хочу 1 — дубль operation_id создал вторую", rows, err)
	}
}

// TestReleaseReturnsToReservationDayNotToday — Release возвращает счётчик дня
// резерва (RETURNING period), а не сегодняшнего дня: процесс упал между
// резервом и вызовом до полуночи, возврат пришёл на следующий день — уменьшать
// сегодняшний счётчик нельзя (блок 17.3).
func TestReleaseReturnsToReservationDayNotToday(t *testing.T) {
	pool := newTestPool(t)
	store := NewBudgetStore(pool)
	ctx := context.Background()
	const op = "model:turn:1736:1:0:0"

	// Вчерашний застрявший резерв + оба счётчика (вчерашний держит резерв).
	if _, err := pool.Exec(ctx, `
		INSERT INTO budget_reservations (operation_id, attempt, pool, reserved, period, created_at)
		VALUES ($1, 0, $2, '{"calls":1}'::jsonb, CURRENT_DATE - 1, now() - interval '25 hours')`,
		op, BudgetPoolConversation); err != nil {
		t.Fatalf("вчерашний резерв не посеян: %v", err)
	}
	// Вчерашний держит застрявший резерв (1), сегодняшний — свой независимый (1).
	for _, day := range []string{"CURRENT_DATE - 1", "CURRENT_DATE"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO budget_counters (pool, period, reserved_calls, updated_at)
			VALUES ($1, `+day+`, 1, now())
			ON CONFLICT (pool, period) DO UPDATE SET reserved_calls = 1`, BudgetPoolConversation); err != nil {
			t.Fatalf("счётчик %s не посеян: %v", day, err)
		}
	}

	if err := store.Release(ctx, op, BudgetPoolConversation, 1); err != nil {
		t.Fatalf("Release не удался: %v", err)
	}

	counter := func(day string) int64 {
		var n int64
		if err := pool.QueryRow(ctx,
			`SELECT reserved_calls FROM budget_counters WHERE pool = $1 AND period = `+day,
			BudgetPoolConversation).Scan(&n); err != nil {
			t.Fatalf("счётчик %s не читается: %v", day, err)
		}
		return n
	}
	if got := counter("CURRENT_DATE - 1"); got != 0 {
		t.Fatalf("счётчик дня резерва %d, хочу 0 — резерв не возвращен в свой день", got)
	}
	if got := counter("CURRENT_DATE"); got != 1 {
		t.Fatalf("сегодняшний счётчик %d, хочу 1 — Release тронул чужой день", got)
	}
	var rows int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM budget_reservations WHERE operation_id = $1`, op).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("резерв не удалён: rows=%d err=%v", rows, err)
	}
}

// TestRecordActualUsageCachedTokens — наблюдаемость кэша префикса (решение
// 2026-11-02): ago отдаёт CacheReadInputTokens, middleware пишет его в actual
// отдельным полем, не смешивая с суммой токенов.
func TestRecordActualUsageCachedTokens(t *testing.T) {
	pool := newTestPool(t)
	store := NewBudgetStore(pool)
	ctx := context.Background()
	const op = "model:turn:2001:0:0"

	if ok, err := store.Reserve(ctx, op, 0, BudgetPoolConversation, 1, 0); err != nil || !ok {
		t.Fatalf("резерв не прошёл: ok=%v err=%v", ok, err)
	}
	cached := int64(64)
	if err := store.RecordActualUsage(ctx, op, BudgetPoolConversation, ActualUsage{
		Tokens: 120, Calls: 1, CachedTokens: &cached,
	}); err != nil {
		t.Fatalf("actual не записан: %v", err)
	}
	var tokens, cachedOut int64
	var cachedPresent bool
	if err := pool.QueryRow(ctx,
		`SELECT (actual->>'tokens')::bigint, (actual->>'cached_tokens')::bigint,
		        actual ? 'cached_tokens'
		   FROM budget_reservations WHERE operation_id = $1`, op).
		Scan(&tokens, &cachedOut, &cachedPresent); err != nil {
		t.Fatalf("actual не читается: %v", err)
	}
	if tokens != 120 || !cachedPresent || cachedOut != 64 {
		t.Fatalf("actual = tokens %d, cached %d (present %v), хочу 120/64", tokens, cachedOut, cachedPresent)
	}

	// Провайдер не сообщил кэш — поля нет (неизвестно ≠ полный промах).
	const op2 = "model:turn:2001:0:1"
	if ok, err := store.Reserve(ctx, op2, 0, BudgetPoolConversation, 1, 0); err != nil || !ok {
		t.Fatalf("резерв 2 не прошёл: ok=%v err=%v", ok, err)
	}
	if err := store.RecordActual(ctx, op2, BudgetPoolConversation, 50, 1, false); err != nil {
		t.Fatalf("actual 2 не записан: %v", err)
	}
	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT actual ? 'cached_tokens' FROM budget_reservations WHERE operation_id = $1`, op2).Scan(&present); err != nil {
		t.Fatalf("actual 2 не читается: %v", err)
	}
	if present {
		t.Fatalf("фантомное cached_tokens без данных провайдера")
	}
}
