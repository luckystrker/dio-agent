//go:build db

// Приёмка 17.12, раунд 2: сценарии сведения, оставшиеся за бортом раунда 1.
// (1) Один и тот же memory item — основание одинакового аспекта у ДВУХ
// дублей: UPDATE stance_basis с NOT EXISTS-щитом сравнивает со снимком
// оператора, обе строки проходят проверку и переезжают победителю —
// duplicate key stance_basis_pair_idx, миграция 000011 откатывается целиком.
// (2) Unicode-форма: ТЗ требует NFC в нормализации имени; сгенерированная
// колонка name_key не приводит форму — легаси-строка в NFD («Алёна» в
// разложенной форме) и NFC-записи дают разные ключи: дубль не сводится
// миграцией и не находится поиском.
package memory

import (
	"context"
	"testing"

	"golang.org/x/text/unicode/norm"

	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/migrations"
)

// TestAcceptEntityMigrationSharedBasisMemory — у канонической сущности
// stance 'general' с основанием m2; у двух дублей — stance 'general' с ОДНИМ
// И ТЕМ ЖЕ основанием m1 (одна запись памяти — основание станс обоих дублей:
// законный расклад до 17.12, Upsert не требует связи памяти с сущностью).
// Миграция обязана сводить без падения: итог — один stance с основаниями
// {m1, m2}.
func TestAcceptEntityMigrationSharedBasisMemory(t *testing.T) {
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
	keep := seed("Данила")   // min(id) — каноническая, stance 'general' с m2
	dup1 := seed("  данила") // дубль 1, stance 'general' с m1
	dup2 := seed("ДАНИЛА ")  // дубль 2, stance 'general' с тем же m1

	prov := seedRawProvenance(t, ctx, pool, []string{scopeA})
	m1 := seedRawItem(t, ctx, pool, scopeA, "Данила принёс лампу", "active", prov)
	m2 := seedRawItem(t, ctx, pool, scopeA, "Данила обещал вернуться", "active", prov)

	stance := func(entity int64, summary string) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO stances (entity_id, aspect, scope_id, summary) VALUES ($1, 'general', $2, $3) RETURNING id`,
			entity, scopeA, summary).Scan(&id); err != nil {
			t.Fatalf("stances(%d): %v", entity, err)
		}
		return id
	}
	kSt := stance(keep, "мнение канонической")
	s1 := stance(dup1, "мнение дубля 1")
	s2 := stance(dup2, "мнение дубля 2")

	for _, pair := range [][2]int64{{kSt, m2}, {s1, m1}, {s2, m1}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO stance_basis (stance_id, memory_id) VALUES ($1, $2)`, pair[0], pair[1]); err != nil {
			t.Fatalf("stance_basis(%v): %v", pair, err)
		}
	}

	if _, err := runtime.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("миграция 000011 не свела основания с общим memory_id (деплой на такой базе упадёт): %v", err)
	}

	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE scope_id = $1`, scopeA).Scan(&total); err != nil {
		t.Fatalf("сущности не читаются: %v", err)
	}
	if total != 1 {
		t.Fatalf("после миграции сущностей = %d, хочу 1", total)
	}
	var basis int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM stance_basis b JOIN stances s ON s.id = b.stance_id
		 WHERE s.entity_id = $1 AND s.aspect = 'general'`, keep).Scan(&basis); err != nil {
		t.Fatalf("stance_basis не читается: %v", err)
	}
	if basis != 2 {
		t.Errorf("оснований у сводной stance = %d, хочу 2 ({m1, m2}, повтор m1 схлопнут)", basis)
	}
}

// TestAcceptEntityNFDNameSameEntity — легаси-строка в NFD-форме («Алёна» с
// разложённой «ё»: е + combining diaeresis) и обычная NFC-запись — одна
// сущность по ТЗ (NFC в нормализации). Миграция обязана свести такие дубли,
// поиск — находить.
func TestAcceptEntityNFDNameSameEntity(t *testing.T) {
	pool := newEntityPoolAt(t, "000010")
	ctx := context.Background()

	var legacyID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO entities (kind, canonical_name, scope_id) VALUES ('person', $1, $2) RETURNING id`,
		norm.NFD.String("Алёна"), scopeA).Scan(&legacyID); err != nil {
		t.Fatalf("легаси NFD-сущность не вставлена: %v", err)
	}

	if _, err := runtime.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("миграция на NFD-легаси упала: %v", err)
	}

	// Запись через код (FindOrCreate всегда пишет NFC): та же сущность.
	found, err := (EntityRepo{}).FindOrCreate(ctx, pool, EntityPerson, "Алёна", scopeA)
	if err != nil {
		t.Fatalf("FindOrCreate(Алёна): %v", err)
	}
	if found.ID != legacyID {
		var n int
		_ = pool.QueryRow(ctx,
			`SELECT count(*) FROM entities WHERE scope_id = $1`, scopeA).Scan(&n)
		t.Fatalf("«Алёна» NFC и «Алёна» NFD — разные сущности (%d против %d, всего %d): "+
			"Unicode-форма не в нормализации name_key", found.ID, legacyID, n)
	}
}

// TestAcceptEntityMigrationKeepsScopesSeparate — сведение дубликатов идёт
// ВНУТРИ скоупа: одинаковое имя в двух чатах — две легитимные сущности
// (§8.1, §18: глобальной уникальности имени нет). Миграция обязана свести
// дубль внутри скоупа и не тронуть тёзку из другого скоупа.
func TestAcceptEntityMigrationKeepsScopesSeparate(t *testing.T) {
	pool := newEntityPoolAt(t, "000010")
	ctx := context.Background()

	seed := func(name, scope string) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO entities (kind, canonical_name, scope_id) VALUES ('person', $1, $2) RETURNING id`,
			name, scope).Scan(&id); err != nil {
			t.Fatalf("сущность %q/%q не вставлена: %v", name, scope, err)
		}
		return id
	}
	keepA := seed("Алёна", scopeA)
	dupA := seed("  АЛЁНА ", scopeA) // дубль внутри скоупа A
	keepB := seed("Алёна", scopeB)   // тёзка в другом скоупе — НЕ дубль

	if _, err := runtime.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("миграция упала на тёзках из разных скоупов: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE scope_id = $1`, scopeA).Scan(&n); err != nil {
		t.Fatalf("сущности скоупа A не читаются: %v", err)
	}
	if n != 1 {
		t.Errorf("в скоупе A осталось %d сущностей, хочу 1 (дубль %d сведён в %d)", n, dupA, keepA)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE scope_id = $1`, scopeB).Scan(&n); err != nil {
		t.Fatalf("сущности скоупа B не читаются: %v", err)
	}
	if n != 1 {
		t.Errorf("в скоупе B осталось %d сущностей, хочу 1 — тёзка другого скоупа не тронута", n)
	}
	var keptB int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM entities WHERE scope_id = $1`, scopeB).Scan(&keptB); err != nil {
		t.Fatalf("каноническая скоупа B не читается: %v", err)
	}
	if keptB != keepB {
		t.Errorf("скоуп B: каноническая = %d, хочу исходную %d", keptB, keepB)
	}

	// Уникальность per-scope: то же имя в третьем скоупе создаётся свободно.
	third, err := (EntityRepo{}).FindOrCreate(ctx, pool, EntityPerson, "алёна", "chat:333")
	if err != nil {
		t.Fatalf("FindOrCreate(алёна, третий скоуп): %v", err)
	}
	if third.ID == keepA || third.ID == keepB {
		t.Errorf("FindOrCreate в новом скоупе вернул сущность чужого скоупа: %d", third.ID)
	}
}
