//go:build db

// БД-тесты ограниченного заимствования соединения (замечание приёмки к 18.5):
// pgx v5 не имеет настройки AcquireTimeout пула, acquire ждёт строго по
// контексту вызова; контексты циклов — без дедлайна. При насыщении пула
// (все MaxConns заняты) ожидание обязано превращаться в обычную ошибку
// запроса за ограниченное время — её подхватывают существующие ретраи.
package runtime

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestPoolAcquireBoundedUnderSaturation — все соединения пула заняты:
// третий запрос обязан упасть ошибкой дедлайна за ограниченное время,
// а не висеть в acquire неограниченно.
func TestPoolAcquireBoundedUnderSaturation(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	ctx := context.Background()
	pool, err := NewPool(ctx, dsn, 2) // маленький пул: насытить руками
	if err != nil {
		t.Skipf("PostgreSQL недоступен по TEST_DATABASE_URL=%q — БД-тест пропущен: %v", dsn, err)
	}
	t.Cleanup(pool.Close)

	hold1, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("первое соединение не заимствовано: %v", err)
	}
	defer hold1.Release()
	hold2, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("второе соединение не заимствовано: %v", err)
	}
	defer hold2.Release()

	done := make(chan error, 1)
	go func() {
		var v int
		done <- pool.QueryRow(ctx, `SELECT 1`).Scan(&v)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("запрос прошёл при насыщенном пуле — тест не насытил пул")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ожидаю ошибку дедлайна ожидания соединения, получено: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("запрос висит в ожидании соединения при насыщении пула дольше 15с — acquire не ограничен по времени (замечание 18.5)")
	}
}

// TestPoolRowsAndTxReturnConnections — обёртка возвращает соединения:
// строки (вычитка/Close/Scan) и транзакции (Commit/Rollback) не текут.
// Регрессионный страж механики обёртки (pgxpool уже даёт эту семантику —
// обёртка обязана её сохранить).
func TestPoolRowsAndTxReturnConnections(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	ctx := context.Background()
	pool, err := NewPool(ctx, dsn, 2)
	if err != nil {
		t.Skipf("PostgreSQL недоступен по TEST_DATABASE_URL=%q — БД-тест пропущен: %v", dsn, err)
	}
	t.Cleanup(pool.Close)

	acquired := func() int32 { return pool.Stat().AcquiredConns() }

	// Полная вычитка строк — соединение вернулось.
	rows, err := pool.Query(ctx, `SELECT generate_series(1, 3)`)
	if err != nil {
		t.Fatalf("запрос не прошёл: %v", err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("строки не дочитаны: %v", err)
	}
	if n := acquired(); n != 0 {
		t.Fatalf("после вычитки строк занято соединений %d, хочу 0", n)
	}

	// Явный Close (ранний выход из выборки) — соединение вернулось.
	rows2, err := pool.Query(ctx, `SELECT generate_series(1, 1000)`)
	if err != nil {
		t.Fatalf("запрос не прошёл: %v", err)
	}
	rows2.Close()
	if n := acquired(); n != 0 {
		t.Fatalf("после Close строк занято соединений %d, хочу 0", n)
	}

	// QueryRow — соединение возвращается по Scan.
	var v int
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&v); err != nil || v != 1 {
		t.Fatalf("QueryRow: v=%d err=%v", v, err)
	}
	if n := acquired(); n != 0 {
		t.Fatalf("после Scan занято соединений %d, хочу 0", n)
	}

	// Транзакция: Commit возвращает соединение.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("транзакция не открыта: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("exec в транзакции: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := acquired(); n != 0 {
		t.Fatalf("после Commit занято соединений %d, хочу 0", n)
	}

	// Rollback (включая defer-повтор после закрытия) не дважды возвращает.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("транзакция не открыта: %v", err)
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := tx2.Rollback(ctx); err == nil {
		t.Fatalf("повторный Rollback должен давать ErrTxClosed")
	}
	if n := acquired(); n != 0 {
		t.Fatalf("после Rollback занято соединений %d, хочу 0", n)
	}
}
