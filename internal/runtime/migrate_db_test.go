//go:build db

// БД-тест раннера миграций (требование 9.1): применение полной схемы §18
// на чистой базе дважды — без ошибок, второй прогон ничего не применяет.
// Запускается отдельно от юнит-гейта:
//
//	go test -tags db ./...
//
// против Postgres из docker-compose (TEST_DATABASE_URL переопределяет DSN).
// Тест создаёт собственную тестовую базу, а после прогона удаляет её:
// разработческая база dio не трогается.
package runtime

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luckystrker/dio-agent/migrations"
)

const defaultTestDSN = "postgres://dio:dio@localhost:5432/dio?sslmode=disable"

// Все таблицы §18 (плюс personas — версии персоны §7.1).
var spec18Tables = []string{
	"social_events", "turns", "delivery_outbox", "runtime_state",
	"budget_reservations", "budget_counters",
	"character_state", "relationships",
	"entities", "entity_aliases", "memory_items", "memory_links",
	"sources", "memory_sources", "memory_derivations",
	"provenance", "stances", "stance_basis",
	"intentions", "event_streams", "task_checkpoints", "personas",
	// 9.3: эмбеддинги семантического поиска (§10.4, §18 — вспомогательная
	// таблица, вектор без размерности, см. migrations/000007_embeddings.sql).
	"memory_embeddings",
}

func TestMigrationsApplyTwiceOnCleanDatabase(t *testing.T) {
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

	// Отдельная чистая база на прогон теста: создать → использовать → удалить.
	dbName := fmt.Sprintf("dio_migrate_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("тестовая база не создана: %v", err)
	}
	t.Cleanup(func() {
		// FORCE: дожить соединения и удалить, даже если пул не успел закрыться.
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
	defer pool.Close()

	// 1. Первое применение на чистой базе: применяются все миграции.
	first, err := Migrate(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatalf("первое применение миграций: %v", err)
	}
	if first.Applied != 17 || first.Skipped != 0 {
		t.Errorf("первый прогон: applied=%d skipped=%d, хочу 17/0", first.Applied, first.Skipped)
	}

	// 2. Повторное применение: идемпотентно, без ошибок и без записей.
	second, err := Migrate(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatalf("второе применение миграций (идемпотентность): %v", err)
	}
	if second.Applied != 0 || second.Skipped != 16 {
		t.Errorf("второй прогон: applied=%d skipped=%d, хочу 0/17", second.Applied, second.Skipped)
	}

	// Все миграции отмечены в schema_migrations.
	var appliedCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&appliedCount); err != nil {
		t.Fatalf("schema_migrations не читается: %v", err)
	}
	if appliedCount != 16 {
		t.Errorf("записей в schema_migrations = %d, хочу 16", appliedCount)
	}

	// Частичный индекс очереди ходов (16.9): worker опрашивает
	// turns WHERE status='created' каждые 500 мс — без индекса seq scan.
	var idxRegclass *string
	if err := pool.QueryRow(ctx, "SELECT to_regclass($1)", "public.turns_created_partial_idx").Scan(&idxRegclass); err != nil {
		t.Fatalf("to_regclass(turns_created_partial_idx): %v", err)
	}
	if idxRegclass == nil {
		t.Error("частичный индекс turns_created_partial_idx отсутствует (16.9)")
	}

	// Предикат частичности (мутационная находка приёмки, М10): индекс обязан
	// покрывать именно очередь created. Предикат по applied рос бы с историей,
	// а запрос очереди worker снова уходил бы в seq scan — молча.
	var idxdef string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_indexdef(c.oid) FROM pg_class c
		JOIN pg_index i ON i.indexrelid = c.oid
		WHERE c.relname = 'turns_created_partial_idx'`).Scan(&idxdef); err != nil {
		t.Fatalf("определение индекса очереди не читается: %v", err)
	}
	if !strings.Contains(idxdef, "WHERE (status = 'created'") {
		t.Errorf("индекс очереди не частичный по created: %s", idxdef)
	}

	// Полная схема §18 на месте.
	for _, table := range spec18Tables {
		var regclass *string
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1)", "public."+table).Scan(&regclass); err != nil {
			t.Fatalf("to_regclass(%s): %v", table, err)
		}
		if regclass == nil {
			t.Errorf("таблица %s из §18 отсутствует", table)
		}
	}

	// Extension vector включён.
	var extName string
	if err := pool.QueryRow(ctx, "SELECT extname FROM pg_extension WHERE extname = 'vector'").Scan(&extName); err != nil {
		t.Errorf("extension vector не установлен: %v", err)
	}

	// Гарантии событийного журнала (§12.1): запись, однократность, append-only.
	if _, err := pool.Exec(ctx,
		`INSERT INTO event_streams (stream_id, seq, type, payload) VALUES ('s-test', 1, 'turn_started', '{}')`); err != nil {
		t.Fatalf("событие журнала не записалось: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO event_streams (stream_id, seq, type) VALUES ('s-test', 1, 'model_output')`); err == nil {
		t.Error("повторная запись UNIQUE(stream_id, seq) должна падать — это однократность применения хода (инвариант 4)")
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO event_streams (stream_id, seq, type) VALUES ('s-test', 2, 'model_output')`); err != nil {
		t.Errorf("следующий seq того же stream должен писаться: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE event_streams SET type = 'hacked' WHERE stream_id = 's-test'`); err == nil {
		t.Error("UPDATE event_streams должен быть запрещён: журнал append-only (§18, инвариант 13)")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE должен падать с объяснением append-only, получено: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM event_streams WHERE stream_id = 's-test'`); err == nil {
		t.Error("DELETE event_streams должен быть запрещён: журнал append-only")
	}
}

// swapDatabaseInDSN подменяет имя базы в postgres://-URL.
func swapDatabaseInDSN(dsn, dbName string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return "", fmt.Errorf("ожидаю postgres://-URL, получено %q", dsn)
	}
	if u.Path == "" {
		return "", fmt.Errorf("в DSN нет имени базы: %q", dsn)
	}
	u.Path = "/" + dbName
	return u.String(), nil
}
