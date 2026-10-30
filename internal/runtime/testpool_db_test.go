//go:build db

// Общий помощник БД-тестов адаптеров (требование 9.2): отдельная чистая база
// на прогон — создать, применить схему, удалить после теста. Запуск:
//
//	go test -tags db ./internal/runtime/
//
// против Postgres из docker-compose; TEST_DATABASE_URL переопределяет DSN
// (на машине разработчика Postgres docker-compose занимает 5433 — см. 9.1).
// Недоступная база — t.Skip с понятной причиной, юнит-гейт это не задевает.
package runtime

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luckystrker/dio-agent/migrations"
)

// newTestPool создаёт пул к свежей тестовой базе с полной схемой §18.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL недоступен по TEST_DATABASE_URL=%q — БД-тест пропущен: %v", dsn, err)
	}
	defer admin.Close(ctx)

	dbName := fmt.Sprintf("dio_store_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("тестовая база не создана: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)")
	})

	testDSN, err := swapDatabaseInDSN(dsn, dbName)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL не разобран как URL: %v", err)
	}

	pool, err := NewPool(ctx, testDSN)
	if err != nil {
		t.Fatalf("пул к тестовой базе не создан: %v", err)
	}
	t.Cleanup(pool.Close)

	result, err := Migrate(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatalf("схема не применена: %v", err)
	}
	if result.Applied != 8 || result.Skipped != 0 {
		t.Fatalf("миграции: applied=%d skipped=%d, хочу 8/0", result.Applied, result.Skipped)
	}
	return pool
}
