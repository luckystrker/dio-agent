// Суточная сверка застрявших резервов бюджета (§16, блок 17.3): reserved_calls —
// храповик, и резерв без фактического usage (процесс упал между резервом и
// вызовом, отмена вне middleware) оставался в счётчике навсегда — в истории
// проекта случался ручной сброс. Сверка раз в сутки в такте планировщика
// возвращает такие резервы в счётчики их дней с WARNING-логом: потолок дня
// не выжигается сбоями.
package background

import (
	"context"
	"time"
)

// staleReserveAge — возраст, после которого неоплаченный резерв (actual IS
// NULL) считается застрявшим и возвращается сверкой.
const staleReserveAge = 24 * time.Hour

// sweepStaleReserves — разовая суточная сверка (сторож lastReserveSweep):
// удаляет резервы без actual старше суток, возвращая их в счётчики своих
// дней. Ошибка сверки не роняет такт — это гигиена, а не модельная работа.
// Часы читаются ДО захвата мьютекса: паника poisoned-зависимости не должна
// оставлять замок занятым (изоляция паник 16.2).
func (s *Scheduler) sweepStaleReserves(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	due := s.lastReserveSweep.IsZero() || !now.Before(s.lastReserveSweep.Add(staleReserveAge))
	if due {
		s.lastReserveSweep = now
	}
	s.mu.Unlock()
	if !due {
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.logError("бюджет: сверка застрявших резервов не открыта: %v", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		DELETE FROM budget_reservations
		WHERE actual IS NULL AND created_at < now() - make_interval(secs => $1)
		RETURNING pool, period, coalesce((reserved->>'calls')::bigint, 1)`,
		staleReserveAge.Seconds())
	if err != nil {
		s.logError("бюджет: сверка застрявших резервов не выполнена: %v", err)
		return
	}
	type returned struct {
		pool   string
		period time.Time
		calls  int64
	}
	var batch []returned
	for rows.Next() {
		var r returned
		if err := rows.Scan(&r.pool, &r.period, &r.calls); err != nil {
			rows.Close()
			s.logError("бюджет: сверка застрявших резервов не дочитана: %v", err)
			return
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logError("бюджет: сверка застрявших резервов не дочитана: %v", err)
		return
	}
	for _, r := range batch {
		if _, err := tx.Exec(ctx, `
			UPDATE budget_counters
			SET reserved_calls = GREATEST(0, reserved_calls - $3), updated_at = now()
			WHERE pool = $1 AND period = $2`, r.pool, r.period, r.calls); err != nil {
			s.logError("бюджет: счётчик дня не возвращён сверкой: %v", err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		s.logError("бюджет: сверка застрявших резервов не зафиксирована: %v", err)
		return
	}
	if len(batch) > 0 {
		var calls int64
		for _, r := range batch {
			calls += r.calls
		}
		s.logWarn("бюджет: сверка вернула застрявших резервов: %d (%d вызовов, старше %v без actual) в счётчики их дней (§16)",
			len(batch), calls, staleReserveAge)
	}
}
