//go:build db

// Общий помощник БД-тестов веб-слоя (этап 36.1): отдельная чистая база на
// прогон, полная схема, удаление после теста. Недоступная база — t.Skip:
// юнит-гейт сборкой с тегом db не задевается.
//
// Пакет web импортируется рантаймом (конфигурация WEB_*), поэтому помощник
// намеренно не тянет internal/runtime (иначе тестовый import cycle): пул —
// pgxpool, миграции — файлы migrations.FS по порядку.
package web

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luckystrker/dio-agent/migrations"
)

const defaultTestDSN = "postgres://dio:dio@localhost:5432/dio?sslmode=disable"

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

	dbName := fmt.Sprintf("dio_web_test_%d", time.Now().UnixNano())
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
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("пул к тестовой базе не создан: %v", err)
	}
	t.Cleanup(pool.Close)

	if applied := applyMigrations(t, ctx, pool); applied != 24 {
		t.Fatalf("миграции: применено %d, хочу 24", applied)
	}
	return pool
}

// applyMigrations применяет migrations.FS по порядку имён (как
// internal/runtime.Migrate, но без импорта рантайма).
func applyMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("schema_migrations не создана: %v", err)
	}
	names, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("миграции не читаются: %v", err)
	}
	var files []string
	for _, entry := range names {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	applied := 0
	for _, name := range files {
		sql, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("миграция %s не читается: %v", name, err)
		}
		script := "BEGIN;\n" + string(sql) +
			"\nINSERT INTO schema_migrations (name) VALUES ('" + strings.ReplaceAll(name, "'", "''") + "');\nCOMMIT;"
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("соединение для миграции %s: %v", name, err)
		}
		_, execErr := conn.Conn().PgConn().Exec(ctx, script).ReadAll()
		conn.Release()
		if execErr != nil {
			t.Fatalf("миграция %s не применена: %v", name, execErr)
		}
		applied++
	}
	return applied
}

func swapDatabaseInDSN(dsn, dbName string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, dbName), nil
}
