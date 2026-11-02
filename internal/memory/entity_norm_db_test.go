//go:build db

// 17.12: нормализация имён сущностей и уникальность (scope, имя). До фикса
// FindOrCreate искал по lower(canonical_name) без нормализации и без
// уникального индекса: «Алёна»/«Алена», «Данила  К.»/«Данила К.» — разные
// сущности, карточка и память делились, гонка двух ходов создавала дубль.
package memory

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/migrations"
)

// TestEntityNormalizationOneEntity — варианты написания одного имени
// (регистр, ё/е, внутренние пробелы, хвостовые пробелы) дают одну сущность.
func TestEntityNormalizationOneEntity(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	entities := EntityRepo{}

	first, err := entities.FindOrCreate(ctx, pool, EntityPerson, "Алёна", scopeA)
	if err != nil {
		t.Fatalf("FindOrCreate(Алёна): %v", err)
	}
	same, err := entities.FindOrCreate(ctx, pool, EntityPerson, "  АЛЕНА  ", scopeA)
	if err != nil {
		t.Fatalf("FindOrCreate(АЛЕНА): %v", err)
	}
	if same.ID != first.ID {
		t.Fatalf("«Алёна» и «АЛЕНА» — разные сущности: %d против %d", same.ID, first.ID)
	}

	spaced, err := entities.FindOrCreate(ctx, pool, EntityPerson, "Данила  К.", scopeA)
	if err != nil {
		t.Fatalf("FindOrCreate(Данила  К.): %v", err)
	}
	sameSpaced, err := entities.FindOrCreate(ctx, pool, EntityPerson, "данила К.", scopeA)
	if err != nil {
		t.Fatalf("FindOrCreate(данила К.): %v", err)
	}
	if sameSpaced.ID != spaced.ID {
		t.Fatalf("«Данила  К.» и «данила К.» — разные сущности: %d против %d", sameSpaced.ID, spaced.ID)
	}

	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE scope_id = $1`, scopeA).Scan(&total); err != nil {
		t.Fatalf("сущности не читаются: %v", err)
	}
	if total != 2 {
		t.Fatalf("сущностей в скоупе %d, хочу 2 (Алёна и Данила К.)", total)
	}
}

// TestEntityConcurrentCreateNoDuplicate — гонка двух ходов: обе транзакции
// не находят сущность и вставляют; уникальность (scope, имя) сводит их в одну.
func TestEntityConcurrentCreateNoDuplicate(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	entities := EntityRepo{}

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("tx1: %v", err)
	}
	defer tx1.Rollback(ctx)
	e1, err := entities.FindOrCreate(ctx, tx1, EntityTopic, "Гоша", scopeA)
	if err != nil {
		t.Fatalf("FindOrCreate(tx1): %v", err)
	}

	// Параллельный ход: своего SELECT ещё не видел вставки tx1.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("tx2: %v", err)
	}
	defer tx2.Rollback(ctx)
	type res struct {
		e   Entity
		err error
	}
	done := make(chan res, 1)
	go func() {
		e, err := entities.FindOrCreate(ctx, tx2, EntityTopic, "  гоша ", scopeA)
		done <- res{e, err}
	}()

	// Ждём, пока вторая вставка упрётся в первую, — либо (без уникальности)
	// завершится сама; тогда просто коммитим и сверяем итог.
	var r res
	finished := false
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case r = <-done:
			finished = true
		default:
		}
		if finished {
			break
		}
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("вторая вставка не заблокировалась за 5 с")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}
	if !finished {
		r = <-done
	}
	if r.err != nil {
		t.Fatalf("FindOrCreate(tx2): %v", r.err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit tx2: %v", err)
	}

	if r.e.ID != e1.ID {
		t.Fatalf("гонка создала дубль: %d против %d", r.e.ID, e1.ID)
	}
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE scope_id = $1`, scopeA).Scan(&total); err != nil {
		t.Fatalf("сущности не читаются: %v", err)
	}
	if total != 1 {
		t.Fatalf("сущностей «Гоша» = %d, хочу 1", total)
	}
}

// TestEntityFindOrCreateByAlias — поиск при создании идёт по имени ИЛИ алиасу:
// сокращение («данил» для «Данила») не плодит вторую сущность, если алиас
// известен (алиасы назначает консолидация/ход — §8.1).
func TestEntityFindOrCreateByAlias(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	entities := EntityRepo{}

	main, err := entities.FindOrCreate(ctx, pool, EntityPerson, "Данила", scopeA)
	if err != nil {
		t.Fatalf("FindOrCreate(Данила): %v", err)
	}
	if err := entities.AddAlias(ctx, pool, main.ID, "данил"); err != nil {
		t.Fatalf("AddAlias(данил): %v", err)
	}

	byAlias, err := entities.FindOrCreate(ctx, pool, EntityPerson, "Данил", scopeA)
	if err != nil {
		t.Fatalf("FindOrCreate(Данил): %v", err)
	}
	if byAlias.ID != main.ID {
		t.Fatalf("«Данил» по алиасу не нашёл «Данила»: %d против %d", byAlias.ID, main.ID)
	}
	// ResolveByName тоже находит по алиасу и по варианту имени.
	resolved, err := entities.ResolveByName(ctx, pool, "  ДАНИЛ ", scopeA)
	if err != nil {
		t.Fatalf("ResolveByName(ДАНИЛ): %v", err)
	}
	if len(resolved) != 1 || resolved[0].ID != main.ID {
		t.Fatalf("ResolveByName(ДАНИЛ) = %+v, хочу сущность %d", resolved, main.ID)
	}
}

// newEntityPoolAt — чистая база с миграциями до upTo включительно: тест
// сведения дубликатов сеет «прошлые» данные схемой до 000011 (без name_key
// и уникальности), затем применяет полную схему — как живая база при деплое.
func newEntityPoolAt(t *testing.T, upTo string) *runtime.Pool {
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
	dbName := fmt.Sprintf("dio_entity_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("тестовая база не создана: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)")
	})

	testDSN, err := swapDatabaseInDSN(dsn, dbName)
	if err != nil {
		t.Fatalf("DSN не разобран: %v", err)
	}
	pool, err := runtime.NewPool(ctx, testDSN)
	if err != nil {
		t.Fatalf("пул не создан: %v", err)
	}
	t.Cleanup(pool.Close)

	sub := fstest.MapFS{}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("миграции не читаются: %v", err)
	}
	for _, name := range names {
		if name > upTo {
			continue
		}
		raw, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("миграция %s не читается: %v", name, err)
		}
		sub[name] = &fstest.MapFile{Data: raw}
	}
	if _, err := runtime.Migrate(ctx, pool, sub); err != nil {
		t.Fatalf("миграции до %s не применены: %v", upTo, err)
	}
	return pool
}

// TestEntityMigrationMergesDuplicates — миграция 000011 сводит готовые
// дубликаты: канонической остаётся строка min(id), зависимости (связи памяти,
// алиасы, stances с основаниями, relationships) переезжают на неё.
func TestEntityMigrationMergesDuplicates(t *testing.T) {
	pool := newEntityPoolAt(t, "000010") // схема до 000011: дубли сеются напрямую
	ctx := context.Background()

	seed := func(name string) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO entities (kind, canonical_name, scope_id) VALUES ('person', $1, $2) RETURNING id`,
			name, scopeA).Scan(&id); err != nil {
			t.Fatalf("сущность %q не вставлена: %v", name, err)
		}
		return id
	}
	keep := seed("Данила")   // min(id) — каноническая
	dup := seed("  данила ") // тот же ключ после нормализации
	_ = seed("ДАНИЛА")

	// Зависимости дубликата: связь памяти, алиас, stance с основанием,
	// relationship — всё обязано переехать на каноническую.
	prov := seedRawProvenance(t, ctx, pool, []string{scopeA})
	memID := seedRawItem(t, ctx, pool, scopeA, "Данил обожает старые ламповые приёмники", "active", prov)
	if _, err := pool.Exec(ctx,
		`INSERT INTO memory_links (memory_id, entity_id, role) VALUES ($1, $2, 'subject')`, memID, dup); err != nil {
		t.Fatalf("memory_links: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO entity_aliases (entity_id, alias, scope_id) VALUES ($1, 'Дэн', $2)`, dup, scopeA); err != nil {
		t.Fatalf("entity_aliases: %v", err)
	}
	var dupStance int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO stances (entity_id, aspect, scope_id, summary) VALUES ($1, 'hobby', $2, 'любит лампу') RETURNING id`,
		dup, scopeA).Scan(&dupStance); err != nil {
		t.Fatalf("stances: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO stance_basis (stance_id, memory_id) VALUES ($1, $2)`, dupStance, memID); err != nil {
		t.Fatalf("stance_basis: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO relationships (person_entity_id, scope_id, affinity) VALUES ($1, $2, 0.4)`, dup, scopeA); err != nil {
		t.Fatalf("relationships: %v", err)
	}
	// Stance-конфликт: у канонической уже есть та же пара (aspect, scope) —
	// основание дубликата дополняет её, дубль уходит.
	var keepStance int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO stances (entity_id, aspect, scope_id, summary) VALUES ($1, 'general', $2, 'хороший знакомый') RETURNING id`,
		keep, scopeA).Scan(&keepStance); err != nil {
		t.Fatalf("stances(keep): %v", err)
	}
	var dupGeneral int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO stances (entity_id, aspect, scope_id, summary) VALUES ($1, 'general', $2, 'дубль мнения') RETURNING id`,
		dup, scopeA).Scan(&dupGeneral); err != nil {
		t.Fatalf("stances(dup general): %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO stance_basis (stance_id, memory_id) VALUES ($1, $2)`, dupGeneral, memID); err != nil {
		t.Fatalf("stance_basis(general): %v", err)
	}

	// Миграция доводит базу до текущей схемы (применяет 000011+).
	if _, err := runtime.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("миграция сведения не применилась: %v", err)
	}

	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE scope_id = $1`, scopeA).Scan(&total); err != nil {
		t.Fatalf("сущности не читаются: %v", err)
	}
	if total != 1 {
		t.Fatalf("после миграции сущностей «Данила» = %d, хочу 1", total)
	}
	var kept int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM entities WHERE scope_id = $1`, scopeA).Scan(&kept); err != nil {
		t.Fatalf("каноническая не читается: %v", err)
	}
	if kept != keep {
		t.Fatalf("каноническая = %d, хочу min(id) = %d", kept, keep)
	}

	check := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("проверка %q: %v", sql, err)
		}
		return n
	}
	if n := check(`SELECT count(*) FROM memory_links WHERE entity_id = $1`, keep); n != 1 {
		t.Errorf("связь памяти не переехала на каноническую: %d", n)
	}
	if n := check(`SELECT count(*) FROM entity_aliases WHERE entity_id = $1`, keep); n != 1 {
		t.Errorf("алиас не переехал на каноническую: %d", n)
	}
	if n := check(`SELECT count(*) FROM stances WHERE entity_id = $1 AND aspect = 'hobby'`, keep); n != 1 {
		t.Errorf("stance без пары не переехала целиком: %d", n)
	}
	if n := check(`SELECT count(*) FROM stance_basis b JOIN stances s ON s.id = b.stance_id
		WHERE s.entity_id = $1 AND s.aspect = 'general'`, keep); n != 1 {
		t.Errorf("основание конфликтного stance не дополнило каноническую: %d", n)
	}
	if n := check(`SELECT count(*) FROM stances WHERE entity_id <> $1 AND scope_id = $2`, keep, scopeA); n != 0 {
		t.Errorf("stances дубликата остались: %d", n)
	}
	if n := check(`SELECT count(*) FROM relationships WHERE person_entity_id = $1`, keep); n != 1 {
		t.Errorf("relationship не переехал на каноническую: %d", n)
	}

	// Уникальность держится на уровне схемы: повторный сырой дубль отклоняется.
	if _, err := pool.Exec(ctx,
		`INSERT INTO entities (kind, canonical_name, scope_id) VALUES ('person', 'данила', $1)`, scopeA); err == nil {
		t.Fatal("сырой дубль прошёл — уникальный индекс не работает")
	}
}
