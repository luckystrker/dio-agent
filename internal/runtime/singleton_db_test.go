//go:build db

// БД-тесты singleton (§5.1): замок занят — второй запуск с ошибкой; обрыв
// соединения снаружи (как при сетевом сбое) замечается и докладывается.
package runtime

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func singletonTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("PostgreSQL недоступен по TEST_DATABASE_URL=%q — БД-тест пропущен: %v", dsn, err)
	}
	if err := conn.Ping(context.Background()); err != nil {
		t.Skipf("PostgreSQL недоступен — БД-тест пропущен: %v", err)
	}
	_ = conn.Close(context.Background())
	return dsn
}

// TestSingletonSecondProcessFails — замок занят: второй запуск получает
// ErrSingletonTaken и соединение своё закрывает.
func TestSingletonSecondProcessFails(t *testing.T) {
	dsn := singletonTestDSN(t)
	ctx := context.Background()

	first, err := AcquireSingleton(ctx, dsn)
	if err != nil {
		t.Fatalf("первый замок не взят: %v", err)
	}
	t.Cleanup(func() { _ = first.Release(ctx) })

	second, err := AcquireSingleton(ctx, dsn)
	if !errors.Is(err, ErrSingletonTaken) {
		t.Fatalf("второй замок: %v, хочу ErrSingletonTaken", err)
	}
	if second != nil {
		t.Fatalf("второй замок вернул объект занятого замка")
	}

	// После освобождения замок берётся снова.
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release не удался: %v", err)
	}
	third, err := AcquireSingleton(ctx, dsn)
	if err != nil {
		t.Fatalf("замок не взят после Release: %v", err)
	}
	if err := third.Release(ctx); err != nil {
		t.Fatalf("повторный Release не удался: %v", err)
	}
}

// TestSingletonLostConnectionReported — соединение разорвано со стороны СУБД:
// наблюдение докладывает о потере однократно (§5.1: прекращает новые действия).
func TestSingletonLostConnectionReported(t *testing.T) {
	dsn := singletonTestDSN(t)
	ctx := context.Background()

	single, err := AcquireSingleton(ctx, dsn)
	if err != nil {
		t.Fatalf("замок не взят: %v", err)
	}
	lost := make(chan string, 1)
	single.OnLost(func(reason string) { lost <- reason })
	// Интервал — заодно бюджет проверки «БД жива?» новым соединением
	// (connect+ping ≈ 30–50 мс на локальной машине): 20 мс не укладывался
	// никогда, проверка всегда выглядела сетевым сбоем и доклад не выходил.
	single.StartWatch(200 * time.Millisecond)
	t.Cleanup(func() { _ = single.Release(ctx) })

	// Разрыв со стороны СУБД (админ-соединение Terminates backend).
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("admin-соединение не открыто: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "SELECT pg_terminate_backend($1)", single.PID()); err != nil {
		t.Fatalf("backend не оборван: %v", err)
	}

	select {
	case reason := <-lost:
		if reason == "" {
			t.Fatalf("пустая причина потери")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("потеря singleton не замечена за 10 с")
	}

	// Release после потери — безопасен (no-op, замок умер с соединением).
	if err := single.Release(ctx); err != nil {
		t.Fatalf("Release после потери вернул ошибку: %v", err)
	}
}
