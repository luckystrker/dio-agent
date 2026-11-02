//go:build db

// Приёмка 17.12: сценарии нормализации и сведения дублей, не покрытые
// тестами дева. (1) Заглавная «Ё»: replace('ё','е') в выражении name_key
// выполняется ДО lower() — «Ё» остаётся «ё» после lower, и «Сёма»/«СЁМА»
// рискуют быть разными сущностями. (2) Сведение зависимостей, когда
// ОДИН ключ делят три и более сущностей и одинаковые зависимые строки
// есть у двух ДУБЛЕЙ (не только у канонической): миграция 000011 обязана
// сводить и это — иначе живой деплой падает на уникальном индексе.
package memory

import (
	"context"
	"testing"

	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/migrations"
)

// TestAcceptEntityUppercaseYoSameEntity — «Сёма» и «СЁМА» одна сущность:
// ё→е обязано работать и в верхнем регистре.
func TestAcceptEntityUppercaseYoSameEntity(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	entities := EntityRepo{}

	first, err := entities.FindOrCreate(ctx, pool, EntityPerson, "Сёма", scopeA)
	if err != nil {
		t.Fatalf("FindOrCreate(Сёма): %v", err)
	}
	second, err := entities.FindOrCreate(ctx, pool, EntityPerson, "СЁМА", scopeA)
	if err != nil {
		t.Fatalf("FindOrCreate(СЁМА): %v", err)
	}
	if second.ID != first.ID {
		var key1, key2 string
		_ = pool.QueryRow(ctx, `SELECT name_key FROM entities WHERE id = $1`, first.ID).Scan(&key1)
		_ = pool.QueryRow(ctx, `SELECT name_key FROM entities WHERE id = $1`, second.ID).Scan(&key2)
		t.Fatalf("«Сёма» и «СЁМА» — разные сущности (%d/%d): name_key %q против %q — ё→е не работает в верхнем регистре",
			first.ID, second.ID, key1, key2)
	}
}

// TestAcceptEntityMigrationMergesDupDupStances — три сущности с одним
// name_key; у двух ДУБЛЕЙ stance одного аспекта, у канонической его нет.
// Миграция обязана свести обе (одна переезжает, вторая дополняет основания
// или уходит), а не упасть на stances_entity_aspect_scope_idx.
func TestAcceptEntityMigrationMergesDupDupStances(t *testing.T) {
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
	keep := seed("Данила")   // min(id) — каноническая, stances нет
	dup1 := seed("  данила") // дубль 1 со stance 'general'
	dup2 := seed("ДАНИЛА ")  // дубль 2 со stance 'general'

	prov := seedRawProvenance(t, ctx, pool, []string{scopeA})
	mem1 := seedRawItem(t, ctx, pool, scopeA, "Данила принёс лампу", "active", prov)
	mem2 := seedRawItem(t, ctx, pool, scopeA, "Данила обещал вернуться", "active", prov)

	var st1, st2 int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO stances (entity_id, aspect, scope_id, summary) VALUES ($1, 'general', $2, 'мнение дубля 1') RETURNING id`,
		dup1, scopeA).Scan(&st1); err != nil {
		t.Fatalf("stances(dup1): %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO stances (entity_id, aspect, scope_id, summary) VALUES ($1, 'general', $2, 'мнение дубля 2') RETURNING id`,
		dup2, scopeA).Scan(&st2); err != nil {
		t.Fatalf("stances(dup2): %v", err)
	}
	for _, pair := range [][2]int64{{st1, mem1}, {st2, mem2}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO stance_basis (stance_id, memory_id) VALUES ($1, $2)`, pair[0], pair[1]); err != nil {
			t.Fatalf("stance_basis: %v", err)
		}
	}

	if _, err := runtime.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("миграция 000011 не свела stance-ы двух дублей (деплой на такой базе упадёт): %v", err)
	}

	// Итог: одна сущность, одна stance 'general' с обоими основаниями.
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE scope_id = $1`, scopeA).Scan(&total); err != nil {
		t.Fatalf("сущности не читаются: %v", err)
	}
	if total != 1 {
		t.Fatalf("после миграции сущностей = %d, хочу 1", total)
	}
	var stances, basis int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM stances WHERE entity_id = $1 AND aspect = 'general'`, keep).Scan(&stances); err != nil {
		t.Fatalf("stances не читаются: %v", err)
	}
	if stances != 1 {
		t.Errorf("stance 'general' у канонической = %d, хочу 1", stances)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM stance_basis b JOIN stances s ON s.id = b.stance_id
		 WHERE s.entity_id = $1 AND s.aspect = 'general'`, keep).Scan(&basis); err != nil {
		t.Fatalf("stance_basis не читается: %v", err)
	}
	if basis != 2 {
		t.Errorf("оснований у сводной stance = %d, хочу 2 (оба дубля)", basis)
	}
}

// TestAcceptEntityMigrationMergesDupDupRelationships — то же для
// relationships: PK (person_entity_id, scope_id); у двух дублей отношения
// в одном скоупе, у канонической нет. Миграция обязана сводить без падения.
func TestAcceptEntityMigrationMergesDupDupRelationships(t *testing.T) {
	pool := newEntityPoolAt(t, "000010")
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
	keep := seed("Гоша")
	dup1 := seed("гоша")
	dup2 := seed(" ГОША")

	for _, id := range []int64{dup1, dup2} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO relationships (person_entity_id, scope_id, affinity) VALUES ($1, $2, 0.3)`,
			id, scopeA); err != nil {
			t.Fatalf("relationships(%d): %v", id, err)
		}
	}

	if _, err := runtime.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("миграция 000011 не свела relationships двух дублей (деплой на такой базе упадёт): %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM relationships WHERE person_entity_id = $1`, keep).Scan(&n); err != nil {
		t.Fatalf("relationships не читается: %v", err)
	}
	if n != 1 {
		t.Errorf("relationships у канонической = %d, хочу 1", n)
	}
}
