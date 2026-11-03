//go:build db

// Общий помощник БД-тестов Telegram-слоя (этап 9.5): отдельная чистая база
// на прогон — создать, применить схему (7 миграций), удалить после теста.
// Запуск:
//
//	go test -tags db ./internal/telegram/
//
// против Postgres из docker-compose; TEST_DATABASE_URL переопределяет DSN
// (на машине разработчика Postgres docker-compose занимает 5433 — см. 9.1).
// Недоступная база — t.Skip с понятной причиной, юнит-гейт это не задевает.
package telegram

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/migrations"
)

const defaultTestDSN = "postgres://dio:dio@localhost:5432/dio?sslmode=disable"

// newTestPool создаёт пул к свежей тестовой базе с полной схемой §18.
func newTestPool(t *testing.T) *runtime.Pool {
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

	dbName := fmt.Sprintf("dio_telegram_test_%d", time.Now().UnixNano())
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

	pool, err := runtime.NewPool(ctx, testDSN)
	if err != nil {
		t.Fatalf("пул к тестовой базе не создан: %v", err)
	}
	t.Cleanup(pool.Close)

	result, err := runtime.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatalf("схема не применена: %v", err)
	}
	if result.Applied != 16 || result.Skipped != 0 {
		t.Fatalf("миграции: applied=%d skipped=%d, хочу 16/0", result.Applied, result.Skipped)
	}
	return pool
}

// swapDatabaseInDSN подменяет имя базы в URL (копия помощника 9.2–9.4, чтобы
// пакет telegram не зависел от внутренностей чужих тестов).
func swapDatabaseInDSN(dsn, dbName string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, dbName), nil
}

// rawMessageOf — update личного сообщения (иногда с reply) для тестов.
func rawMessageOf(updateID, chatID int64, text string) []byte {
	return []byte(fmt.Sprintf(`{"update_id":%d,"message":{"message_id":%d,"from":{"id":%d,"first_name":"Коля"},"chat":{"id":%d,"type":"private"},"text":%q}}`,
		updateID, updateID, chatID, chatID, text))
}

// mustIngest сохраняет update, роняя тест при ошибке.
func mustIngest(t *testing.T, pool *runtime.Pool, raw []byte) IngestResult {
	t.Helper()
	res, err := IngestUpdate(context.Background(), pool, raw)
	if err != nil {
		t.Fatalf("ingest не удался: %v", err)
	}
	return res
}
