//go:build db

// БД-тесты идентичности собеседника (M7): ключ сущности — actor_id, а не
// отображаемое имя; имя становится алиасом.
package memory

import (
	"context"
	"testing"
)

// TestEnsurePersonAdoptsLegacyEntity — major ревью: до M7 ключом было
// отображаемое имя. Первый актор с таким именем усыновляет старую сущность
// (его отношения/карточка не обнуляются), второй «Саша» получает свою.
func TestEnsurePersonAdoptsLegacyEntity(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := EntityRepo{}
	scope := "chat:77"

	legacy, err := repo.FindOrCreate(ctx, pool, EntityPerson, "Саша", scope)
	if err != nil {
		t.Fatalf("легаси-сущность: %v", err)
	}
	adopted, err := repo.EnsurePerson(ctx, pool, 2001, "Саша", scope)
	if err != nil {
		t.Fatalf("EnsurePerson(2001): %v", err)
	}
	if adopted.ID != legacy.ID {
		t.Fatalf("легаси-сущность не усыновлена: %d вместо %d", adopted.ID, legacy.ID)
	}
	second, err := repo.EnsurePerson(ctx, pool, 2002, "Саша", scope)
	if err != nil {
		t.Fatalf("EnsurePerson(2002): %v", err)
	}
	if second.ID == legacy.ID {
		t.Fatalf("второй «Саша» слился с усыновлённой сущностью %d", legacy.ID)
	}
}

// сущности: доверие, симпатия и карточка памяти не смешиваются.
func TestEnsurePersonDistinctActorsSameName(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := EntityRepo{}
	scope := "chat:42"

	first, err := repo.EnsurePerson(ctx, pool, 1001, "Саша", scope)
	if err != nil {
		t.Fatalf("EnsurePerson(1001): %v", err)
	}
	second, err := repo.EnsurePerson(ctx, pool, 1002, "Саша", scope)
	if err != nil {
		t.Fatalf("EnsurePerson(1002): %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("два актора с одинаковым именем слились в сущность %d", first.ID)
	}
	if first.CanonicalName != PersonEntityName(1001) || second.CanonicalName != PersonEntityName(1002) {
		t.Fatalf("ключ не actor_id: %q / %q", first.CanonicalName, second.CanonicalName)
	}

	// Смена отображаемого имени находит ту же сущность по actor_id.
	renamed, err := repo.EnsurePerson(ctx, pool, 1001, "Александр", scope)
	if err != nil {
		t.Fatalf("EnsurePerson(1001, новое имя): %v", err)
	}
	if renamed.ID != first.ID {
		t.Fatalf("смена имени создала новую сущность: %d вместо %d", renamed.ID, first.ID)
	}

	// Старое и новое имя сохранены алиасами той же сущности.
	for _, alias := range []string{"Саша", "Александр"} {
		var entityID int64
		if err := pool.QueryRow(ctx,
			`SELECT entity_id FROM entity_aliases WHERE alias = $1 ORDER BY entity_id LIMIT 1`, alias).
			Scan(&entityID); err != nil {
			t.Fatalf("алиас %q не записан: %v", alias, err)
		}
		if entityID != first.ID {
			t.Fatalf("алиас %q принадлежит %d, хочу %d", alias, entityID, first.ID)
		}
	}
}
