//go:build db

// Независимая проверка 18.5 (дополнение к TestPoolAcquireBoundedUnderSaturation):
// граница ожидания соединения обязана работать и на транзакционном пути
// (Begin — путь ingest), и на Exec, а не только в QueryRow; после освобождения
// пула заимствование снова работает (таймаут не «съедает» слот навсегда).
package runtime

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestPoolAcquireBoundedBeginExec(t *testing.T) {
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

	// Begin при насыщении — ошибка дедлайна за ограниченное время.
	doneBegin := make(chan error, 1)
	go func() {
		tx, berr := pool.Begin(ctx)
		if berr == nil {
			_ = tx.Rollback(ctx)
		}
		doneBegin <- berr
	}()
	select {
	case err := <-doneBegin:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Begin при насыщении: %v, хочу ошибку дедлайна ожидания соединения", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Begin висит в ожидании соединения дольше 15с — транзакционный путь не ограничен (18.5)")
	}

	// Exec — та же граница.
	doneExec := make(chan error, 1)
	go func() {
		_, eerr := pool.Exec(ctx, `SELECT 1`)
		doneExec <- eerr
	}()
	select {
	case err := <-doneExec:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Exec при насыщении: %v, хочу ошибку дедлайна ожидания соединения", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Exec висит в ожидании соединения дольше 15с — путь Exec не ограничен (18.5)")
	}

	// После освобождения пул работоспособен.
	hold1.Release()
	hold2.Release()
	var v int
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&v); err != nil || v != 1 {
		t.Fatalf("после освобождения соединений запрос не прошёл: v=%d err=%v", v, err)
	}
}
