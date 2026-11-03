//go:build db

// БД-тест проводки объединения сущностей в консолидацию (этап 32.1–32.2):
// модель предлагает entity_merges по именам эпизода, runtime резолвит имена,
// проверяет и склеивает; повторная консолидация того же эпизода — no-op.
package background

import (
	"fmt"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/memory"
)

func TestConsolidationAppliesEntityMerge(t *testing.T) {
	f, ctx := newFixture(t)
	ids, itemID := f.seedEpisode(t, ctx, bgChatID, "private", 2, f.now.Add(-3*time.Hour))
	scope := memory.ChatScope(fmt.Sprintf("%d", bgChatID))
	repo := memory.EntityRepo{}

	keep, err := repo.FindOrCreate(ctx, f.pool, memory.EntityPerson, "Коля", scope)
	if err != nil {
		t.Fatalf("сущность эпизода: %v", err)
	}
	merge, err := repo.FindOrCreate(ctx, f.pool, memory.EntityPerson, "Николай", scope)
	if err != nil {
		t.Fatalf("вторая сущность: %v", err)
	}
	// Вторая сущность тоже связана с памятью эпизода — иначе runtime её не
	// увидит в словаре имён эпизода (как и stances).
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO memory_links (memory_id, entity_id, role) VALUES ($1, $2, 'subject')`,
		itemID, merge.ID); err != nil {
		t.Fatalf("связь второй сущности: %v", err)
	}

	proposal := consolidationProposal{
		Summary: "Обсудили винил; Коля и Николай — одно лицо.",
		EntityMerges: []entityMergeProposal{{
			Keep: "Коля", Merge: "Николай", Reason: "в эпизоде видно, что это одно лицо",
		}},
	}
	ep := Episode{ChatID: bgChatID, ChatType: "private", EventIDs: ids, LastEventID: ids[len(ids)-1]}
	applied, detail, err := f.sched.applyConsolidation(ctx, ep, proposal)
	if err != nil {
		t.Fatalf("консолидация со склейкой: %v", err)
	}
	if !applied {
		t.Fatal("эпизод должен примениться")
	}
	t.Logf("консолидация: %s", detail)

	var into *int64
	if err := f.pool.QueryRow(ctx, `SELECT merged_into FROM entities WHERE id = $1`, merge.ID).Scan(&into); err != nil {
		t.Fatalf("merged_into не читается: %v", err)
	}
	if into == nil || *into != keep.ID {
		t.Fatalf("сущность эпизода не склеена: merged_into=%v, хочу %d", into, keep.ID)
	}
	var journal int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM entity_merges WHERE keep_id = $1 AND merge_id = $2 AND rolled_back_at IS NULL`,
		keep.ID, merge.ID).Scan(&journal); err != nil {
		t.Fatalf("журнал склейки не читается: %v", err)
	}
	if journal != 1 {
		t.Fatalf("журнал склейки: строк %d, хочу 1", journal)
	}
	var left int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_links WHERE entity_id = $1`, merge.ID).Scan(&left); err != nil {
		t.Fatalf("связи merge не читаются: %v", err)
	}
	if left != 0 {
		t.Fatalf("связи памяти не переехали на keep: осталось %d", left)
	}

	// Поиск по имени поглощённой сущности находит выжившую.
	found, err := repo.ResolveByName(ctx, f.pool, "Николай", scope)
	if err != nil || len(found) != 1 || found[0].ID != keep.ID {
		t.Fatalf("поиск по имени merge должен найти keep: %+v err=%v", found, err)
	}

	// Повтор того же эпизода — no-op: второй склейки и журнальной записи нет.
	appliedAgain, _, err := f.sched.applyConsolidation(ctx, ep, proposal)
	if err != nil {
		t.Fatalf("повторная консолидация: %v", err)
	}
	if appliedAgain {
		t.Fatal("повторная консолидация эпизода должна быть no-op")
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM entity_merges WHERE keep_id = $1`, keep.ID).Scan(&journal); err != nil {
		t.Fatalf("журнал после повтора: %v", err)
	}
	if journal != 1 {
		t.Fatalf("повтор не должен писать новый журнал: %d", journal)
	}
}
