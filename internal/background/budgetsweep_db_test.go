//go:build db

// БД-тест суточной сверки застрявших резервов бюджета (§16, блок 17.3):
// неоплаченные резервы старше суток возвращаются в счётчик своего дня с
// WARNING-логом; оплаченные и свежие не трогаются; повторная сверка в тот же
// день не выполняется.
package background

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// seedReservation — резерв пула background заданного возраста; period — день
// создания (Reserve штампует CURRENT_DATE в момент резерва, стáрый резерв
// принадлежит прошлому дню).
func seedReservation(t *testing.T, ctx context.Context, f *fixture, op string, age time.Duration, withActual bool) {
	t.Helper()
	var actual any
	if withActual {
		actual = `{"calls":1,"unknown":true}`
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO budget_reservations (operation_id, attempt, pool, reserved, actual, period, created_at)
		VALUES ($1, 0, 'background', '{"calls":1}'::jsonb, $2::jsonb, (now() - $3::interval)::date, now() - $3::interval)`,
		op, actual, age.String()); err != nil {
		t.Fatalf("резерв %s не посеян: %v", op, err)
	}
}

// counterOf — reserved_calls счётчика (pool, period=CURRENT_DATE - 1 день).
func yesterdayCounter(t *testing.T, ctx context.Context, f *fixture) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(ctx,
		`SELECT reserved_calls FROM budget_counters WHERE pool = 'background' AND period = CURRENT_DATE - 1`).
		Scan(&n); err != nil {
		t.Fatalf("счётчик позавчерашнего дня не читается: %v", err)
	}
	return n
}

// TestSweepStaleReserves — сверка чистит застрявшие, не трогает оплаченные
// и свежие; вторая сверка в тот же день не выполняется.
func TestSweepStaleReserves(t *testing.T) {
	f, ctx := newFixture(t)

	// Счётчик позавчерашнего дня: 3 резерва (застрявший + оплаченный + ещё один).
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO budget_counters (pool, period, reserved_calls, updated_at)
		 VALUES ('background', CURRENT_DATE - 1, 3, now())`); err != nil {
		t.Fatalf("счётчик не посеян: %v", err)
	}
	seedReservation(t, ctx, f, "model:bg:stale:1", 25*time.Hour, false) // застрявший
	seedReservation(t, ctx, f, "model:bg:paid:1", 25*time.Hour, true)   // оплаченный — неприкосновенен
	seedReservation(t, ctx, f, "model:bg:fresh:1", time.Hour, false)    // свежий — рано

	// Свои часы и лог: сверка видна оператору WARNING-строкой.
	var buf bytes.Buffer
	s, err := New(Deps{
		Pool:        f.pool,
		Journal:     f.journal,
		WAL:         f.wal,
		Checkpoints: f.checkpoints,
		Budget:      f.budget,
		Logger:      log.New(&buf, "", 0),
		Now:         func() time.Time { return f.now },
	}, Options{})
	if err != nil {
		t.Fatalf("планировщик не создан: %v", err)
	}

	if err := s.Tick(ctx); err != nil {
		t.Fatalf("такт со сверкой не удался: %v", err)
	}

	var stale int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM budget_reservations WHERE operation_id = 'model:bg:stale:1'`).Scan(&stale); err != nil {
		t.Fatalf("резервы не читаются: %v", err)
	}
	if stale != 0 {
		t.Fatal("застрявший резерв (25 ч без actual) не возвращён сверкой — день выжжен навсегда (§16)")
	}
	if got := yesterdayCounter(t, ctx, f); got != 2 {
		t.Fatalf("счётчик позавчерашнего дня %d, хочу 2 — сверка вернула не только застрявший резерв", got)
	}
	var paid, fresh int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM budget_reservations WHERE operation_id = 'model:bg:paid:1'`).Scan(&paid)
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM budget_reservations WHERE operation_id = 'model:bg:fresh:1'`).Scan(&fresh)
	if paid != 1 || fresh != 1 {
		t.Fatalf("сверка тронула лишнее: оплаченный=%d свежий=%d, хочу 1 и 1", paid, fresh)
	}
	if !strings.Contains(buf.String(), "застрявш") {
		t.Fatalf("возврат застрявших резервов должен предупреждать оператора (WARNING), лог: %q", buf.String())
	}

	// Разовость за сутки: новый застрявший резерв тем же днём не подбирается.
	buf.Reset()
	seedReservation(t, ctx, f, "model:bg:stale:2", 26*time.Hour, false)
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("повторный такт не удался: %v", err)
	}
	var stale2 int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM budget_reservations WHERE operation_id = 'model:bg:stale:2'`).Scan(&stale2)
	if stale2 != 1 {
		t.Fatal("сверка выполнилась повторно в тот же день — должна быть разовой в сутки")
	}
}
