//go:build db

// БД-тесты объединения сущностей (этап 32, §8.5): склейка переносит всё,
// разные Telegram-аккаунты и разные скоупы приватности — отказ, повтор
// предложения идемпотентен, откат по журналу возвращает состояние как было.
package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// mergeWorld — посев для склейки: две person-сущности одного скоупа, записи
// памяти на каждой, конфликтующий и неконфликтующий stance, отношения.
type mergeWorld struct {
	Keep, Merge Entity
	ItemKeep    int64
	ItemMerge   int64
}

func seedMergeWorld(t *testing.T, ctx context.Context, pool DBTX, scope string) mergeWorld {
	t.Helper()
	repo := EntityRepo{}

	keep, err := repo.EnsurePerson(ctx, pool, 9001, "Коля", scope)
	if err != nil {
		t.Fatalf("сущность keep: %v", err)
	}
	merge, err := repo.FindOrCreate(ctx, pool, EntityPerson, "Николай", scope)
	if err != nil {
		t.Fatalf("сущность merge: %v", err)
	}
	if err := repo.AddAlias(ctx, pool, merge.ID, "Колян"); err != nil {
		t.Fatalf("алиас merge: %v", err)
	}

	provID, err := (SourceRepo{}).EnsureProvenance(ctx, pool, SourceJournalEvent, nextRef("merge"), []string{scope})
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	itemKeep, err := (Items{}).Insert(ctx, pool, NewItem{
		Kind: KindClaim, Text: "Коля собирает виниловые пластинки", Salience: 0.6,
		ProvenanceID: provID,
	}, scope)
	if err != nil {
		t.Fatalf("запись keep: %v", err)
	}
	itemMerge, err := (Items{}).Insert(ctx, pool, NewItem{
		Kind: KindClaim, Text: "Николай любит джаз на пластинках", Salience: 0.5,
		ProvenanceID: provID,
	}, scope)
	if err != nil {
		t.Fatalf("запись merge: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memory_links (memory_id, entity_id, role) VALUES ($1, $2, 'subject')`,
		itemKeep.ID, keep.ID); err != nil {
		t.Fatalf("связь keep: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memory_links (memory_id, entity_id, role) VALUES ($1, $2, 'subject')`,
		itemMerge.ID, merge.ID); err != nil {
		t.Fatalf("связь merge: %v", err)
	}

	// Конфликт аспекта «общение»: у keep и merge по stance, у merge ещё и
	// неконфликтующий «музыка» — он должен переехать целиком.
	if _, err := (Stances{}).Upsert(ctx, pool, StanceInput{
		EntityID: keep.ID, Aspect: "общение", ScopeID: scope, Summary: "ровное",
		Strength: 0.4, Interest: 0.5, BasisMemoryIDs: []int64{itemKeep.ID},
	}); err != nil {
		t.Fatalf("stance keep: %v", err)
	}
	if _, err := (Stances{}).Upsert(ctx, pool, StanceInput{
		EntityID: merge.ID, Aspect: "общение", ScopeID: scope, Summary: "сдержанный",
		Strength: 0.8, Interest: 0.2, BasisMemoryIDs: []int64{itemMerge.ID},
	}); err != nil {
		t.Fatalf("stance merge (конфликт): %v", err)
	}
	if _, err := (Stances{}).Upsert(ctx, pool, StanceInput{
		EntityID: merge.ID, Aspect: "музыка", ScopeID: scope, Summary: "любит винил",
		Strength: 0.7, Interest: 0.9, BasisMemoryIDs: []int64{itemMerge.ID},
	}); err != nil {
		t.Fatalf("stance merge (музыка): %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO relationships (person_entity_id, scope_id, familiarity, affinity, trust)
		VALUES ($1, $2, 0.4, 0.2, 0.5), ($3, $2, 0.6, 0.6, 0.3)`,
		keep.ID, scope, merge.ID); err != nil {
		t.Fatalf("отношения: %v", err)
	}
	return mergeWorld{Keep: keep, Merge: merge, ItemKeep: itemKeep.ID, ItemMerge: itemMerge.ID}
}

// aliasState/linkState/stanceState/relationshipState — снимки для сравнения
// «как было» до склейки и после отката.
func aliasState(t *testing.T, ctx context.Context, pool DBTX, entityID int64) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT alias, scope_id FROM entity_aliases WHERE entity_id = $1 ORDER BY alias, scope_id`, entityID)
	if err != nil {
		t.Fatalf("алиасы не читаются: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var alias, scope string
		if err := rows.Scan(&alias, &scope); err != nil {
			t.Fatalf("строка алиаса: %v", err)
		}
		out = append(out, alias+"|"+scope)
	}
	return out
}

func linkState(t *testing.T, ctx context.Context, pool DBTX, entityID int64) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT memory_id, role FROM memory_links WHERE entity_id = $1 ORDER BY memory_id, role`, entityID)
	if err != nil {
		t.Fatalf("связи не читаются: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		var role string
		if err := rows.Scan(&id, &role); err != nil {
			t.Fatalf("строка связи: %v", err)
		}
		out = append(out, fmt.Sprintf("%d|%s", id, role))
	}
	return out
}

func stanceState(t *testing.T, ctx context.Context, pool DBTX, entityID int64) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT s.aspect, s.scope_id, s.summary, s.strength, s.interest, s.revision,
		       COALESCE(string_agg(b.memory_id::text, ',' ORDER BY b.memory_id), '')
		FROM stances s
		LEFT JOIN stance_basis b ON b.stance_id = s.id
		WHERE s.entity_id = $1
		GROUP BY s.id
		ORDER BY s.aspect, s.scope_id`, entityID)
	if err != nil {
		t.Fatalf("stances не читаются: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var aspect, scope, summary, basis string
		var strength, interest float64
		var revision int
		if err := rows.Scan(&aspect, &scope, &summary, &strength, &interest, &revision, &basis); err != nil {
			t.Fatalf("строка stance: %v", err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%.3f|%.3f|%d|%s",
			aspect, scope, summary, strength, interest, revision, basis))
	}
	return out
}

func relationshipState(t *testing.T, ctx context.Context, pool DBTX, entityID int64) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT scope_id, familiarity, affinity, trust FROM relationships
		 WHERE person_entity_id = $1 ORDER BY scope_id`, entityID)
	if err != nil {
		t.Fatalf("отношения не читаются: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var scope string
		var fam, aff, trust float64
		if err := rows.Scan(&scope, &fam, &aff, &trust); err != nil {
			t.Fatalf("строка отношений: %v", err)
		}
		out = append(out, fmt.Sprintf("%s|%.3f|%.3f|%.3f", scope, fam, aff, trust))
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mergedInto(t *testing.T, ctx context.Context, pool DBTX, id int64) *int64 {
	t.Helper()
	var into *int64
	if err := pool.QueryRow(ctx, `SELECT merged_into FROM entities WHERE id = $1`, id).Scan(&into); err != nil {
		t.Fatalf("merged_into не читается: %v", err)
	}
	return into
}

// TestMergeEntitiesMovesEverything — склейка переносит алиасы (включая
// каноническое имя), связи памяти, stances с основаниями и отношения; журнал
// помнит снимок; поиск по имени/алиасу поглощённой сущности находит выжившую.
func TestMergeEntitiesMovesEverything(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	scope := "chat:501"
	w := seedMergeWorld(t, ctx, pool, scope)
	repo := EntityRepo{}

	res, err := repo.Merge(ctx, pool, EntityMergeRequest{
		KeepID: w.Keep.ID, MergeID: w.Merge.ID,
		Reason: "Николай — то же лицо, что Коля", ProposedBy: "consolidation", Audience: scope,
	})
	if err != nil {
		t.Fatalf("склейка не выполнена: %v", err)
	}
	if res.JournalID <= 0 || res.AlreadyMerged {
		t.Fatalf("итог склейки: %+v", res)
	}
	if got := mergedInto(t, ctx, pool, w.Merge.ID); got == nil || *got != w.Keep.ID {
		t.Fatalf("поглощённая сущность не помечена merged_into=%v, хочу %d", got, w.Keep.ID)
	}

	keepAliases := aliasState(t, ctx, pool, w.Keep.ID)
	for _, want := range []string{"Коля|" + scope, "Колян|" + scope, "Николай|" + scope} {
		found := false
		for _, got := range keepAliases {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("алиас %q не переехал на keep: %v", want, keepAliases)
		}
	}
	if left := aliasState(t, ctx, pool, w.Merge.ID); len(left) != 0 {
		t.Fatalf("алиасы не должны оставаться на merge: %v", left)
	}

	keepLinks := linkState(t, ctx, pool, w.Keep.ID)
	if len(keepLinks) != 2 {
		t.Fatalf("связи памяти не переехали: %v", keepLinks)
	}
	if left := linkState(t, ctx, pool, w.Merge.ID); len(left) != 0 {
		t.Fatalf("связи остались на merge: %v", left)
	}

	// Конфликт аспекта: остаётся keep, основания merge добавляются; неконфликтующий
	// аспект переезжает целиком.
	stances := stanceState(t, ctx, pool, w.Keep.ID)
	var conflict, music bool
	for _, st := range stances {
		if st == fmt.Sprintf("общение|%s|ровное|0.400|0.500|0|%d,%d", scope, w.ItemKeep, w.ItemMerge) {
			conflict = true
		}
		if st == fmt.Sprintf("музыка|%s|любит винил|0.700|0.900|0|%d", scope, w.ItemMerge) {
			music = true
		}
	}
	if !conflict || !music {
		t.Fatalf("stances после склейки не те: %v", stances)
	}
	if left := stanceState(t, ctx, pool, w.Merge.ID); len(left) != 0 {
		t.Fatalf("stances остались на merge: %v", left)
	}

	// Конфликт отношений: familiarity — max, affinity/trust — середина между
	// keep и merge (чужой опыт не перевешивает, но и не теряется).
	rels := relationshipState(t, ctx, pool, w.Keep.ID)
	if len(rels) != 1 || rels[0] != fmt.Sprintf("%s|0.600|0.400|0.400", scope) {
		t.Fatalf("отношения после склейки не те: %v", rels)
	}
	if left := relationshipState(t, ctx, pool, w.Merge.ID); len(left) != 0 {
		t.Fatalf("отношения остались на merge: %v", left)
	}

	var snap []byte
	if err := pool.QueryRow(ctx,
		`SELECT snapshot FROM entity_merges WHERE id = $1`, res.JournalID).Scan(&snap); err != nil {
		t.Fatalf("журнал склейки не читается: %v", err)
	}
	if len(snap) == 0 {
		t.Fatal("журнал склейки должен хранить снимок для отката")
	}

	// Поиск/карточка по имени и алиасу merge находит keep; Get по id merge
	// перенаправляет на выжившую сущность.
	found, err := repo.ResolveByName(ctx, pool, "Николай", scope)
	if err != nil || len(found) != 1 || found[0].ID != w.Keep.ID {
		t.Fatalf("поиск по каноническому имени merge должен найти keep: %+v err=%v", found, err)
	}
	found, err = repo.ResolveByName(ctx, pool, "Колян", scope)
	if err != nil || len(found) != 1 || found[0].ID != w.Keep.ID {
		t.Fatalf("поиск по алиасу merge должен найти keep: %+v err=%v", found, err)
	}
	if got, err := repo.Get(ctx, pool, w.Merge.ID, scope); err != nil || got.ID != w.Keep.ID {
		t.Fatalf("Get по id merge должен вернуть keep: %+v err=%v", got, err)
	}
	if got, err := repo.FindOrCreate(ctx, pool, EntityPerson, "Николай", scope); err != nil || got.ID != w.Keep.ID {
		t.Fatalf("FindOrCreate по имени merge должен найти keep: %+v err=%v", got, err)
	}
}

// TestMergeEntitiesRefusesDifferentTelegramAccounts — разные Telegram user id —
// разные люди: склейка запрещена всегда (§8.5, M7).
func TestMergeEntitiesRefusesDifferentTelegramAccounts(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	scope := "chat:502"
	repo := EntityRepo{}

	a, err := repo.EnsurePerson(ctx, pool, 1001, "Саша", scope)
	if err != nil {
		t.Fatalf("EnsurePerson(1001): %v", err)
	}
	b, err := repo.EnsurePerson(ctx, pool, 1002, "Саша", scope)
	if err != nil {
		t.Fatalf("EnsurePerson(1002): %v", err)
	}

	_, err = repo.Merge(ctx, pool, EntityMergeRequest{
		KeepID: a.ID, MergeID: b.ID, Reason: "одно имя", ProposedBy: "consolidation", Audience: scope,
	})
	if !errors.Is(err, ErrMergeRefused) {
		t.Fatalf("разные Telegram-аккаунты должны отклоняться: %v", err)
	}
	if got := mergedInto(t, ctx, pool, b.ID); got != nil {
		t.Fatalf("отказ не должен помечать сущность merged_into: %v", got)
	}
	var journal int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM entity_merges`).Scan(&journal); err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	if journal != 0 {
		t.Fatalf("отказ не должен писать журнал, строк %d", journal)
	}
}

// TestMergeEntitiesRefusesDifferentScopes — один скоуп приватности: даже
// видимые внутреннему контексту сущности разных скоупов не склеиваются (§9.1).
func TestMergeEntitiesRefusesDifferentScopes(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := EntityRepo{}

	publicEntity, err := repo.FindOrCreate(ctx, pool, EntityTopic, "Ремонт", ScopePublic)
	if err != nil {
		t.Fatalf("публичная сущность: %v", err)
	}
	chatEntity, err := repo.FindOrCreate(ctx, pool, EntityTopic, "Ремонт", "chat:503")
	if err != nil {
		t.Fatalf("сущность чата: %v", err)
	}
	_, err = repo.Merge(ctx, pool, EntityMergeRequest{
		KeepID: publicEntity.ID, MergeID: chatEntity.ID,
		Reason: "одна тема", ProposedBy: "consolidation", Audience: ScopeInternal,
	})
	if !errors.Is(err, ErrMergeRefused) {
		t.Fatalf("разные скоупы приватности должны отклоняться: %v", err)
	}
	if got := mergedInto(t, ctx, pool, chatEntity.ID); got != nil {
		t.Fatalf("отказ не должен помечать сущность merged_into: %v", got)
	}

	// Невидимая в скоупе эпизода сущность — отказ (существование не
	// раскрывается, §9.2).
	a, err := repo.FindOrCreate(ctx, pool, EntityTopic, "Кино", "chat:504")
	if err != nil {
		t.Fatalf("сущность A: %v", err)
	}
	b, err := repo.FindOrCreate(ctx, pool, EntityTopic, "Кино", "chat:505")
	if err != nil {
		t.Fatalf("сущность B: %v", err)
	}
	_, err = repo.Merge(ctx, pool, EntityMergeRequest{
		KeepID: a.ID, MergeID: b.ID, Reason: "одна тема", ProposedBy: "consolidation", Audience: "chat:504",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("невидимая сущность должна давать ErrNotFound: %v", err)
	}
}

// TestMergeEntitiesRefusesPersonWithNonPerson — person с не-person не
// склеиваются: это разные виды идентификаторов.
func TestMergeEntitiesRefusesPersonWithNonPerson(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	scope := "chat:506"
	repo := EntityRepo{}

	person, err := repo.EnsurePerson(ctx, pool, 2001, "Коля", scope)
	if err != nil {
		t.Fatalf("person: %v", err)
	}
	topic, err := repo.FindOrCreate(ctx, pool, EntityTopic, "Винил", scope)
	if err != nil {
		t.Fatalf("topic: %v", err)
	}
	_, err = repo.Merge(ctx, pool, EntityMergeRequest{
		KeepID: person.ID, MergeID: topic.ID,
		Reason: "Коля и винил", ProposedBy: "consolidation", Audience: scope,
	})
	if !errors.Is(err, ErrMergeRefused) {
		t.Fatalf("person с не-person должен отклоняться: %v", err)
	}

	// Сущность сама с собой — тоже отказ.
	_, err = repo.Merge(ctx, pool, EntityMergeRequest{
		KeepID: person.ID, MergeID: person.ID,
		Reason: "сама с собой", ProposedBy: "consolidation", Audience: scope,
	})
	if !errors.Is(err, ErrMergeRefused) {
		t.Fatalf("склейка с самой собой должна отклоняться: %v", err)
	}
}

// TestMergeEntitiesIdempotent — повторное предложение после склейки — no-op:
// второй журнальной записи нет, алиасы/связи не задваиваются.
func TestMergeEntitiesIdempotent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	scope := "chat:507"
	w := seedMergeWorld(t, ctx, pool, scope)
	repo := EntityRepo{}

	first, err := repo.Merge(ctx, pool, EntityMergeRequest{
		KeepID: w.Keep.ID, MergeID: w.Merge.ID,
		Reason: "одно лицо", ProposedBy: "consolidation", Audience: scope,
	})
	if err != nil {
		t.Fatalf("первая склейка: %v", err)
	}
	aliasesBefore := aliasState(t, ctx, pool, w.Keep.ID)
	linksBefore := linkState(t, ctx, pool, w.Keep.ID)

	second, err := repo.Merge(ctx, pool, EntityMergeRequest{
		KeepID: w.Keep.ID, MergeID: w.Merge.ID,
		Reason: "одно лицо", ProposedBy: "consolidation", Audience: scope,
	})
	if err != nil {
		t.Fatalf("повторная склейка должна быть no-op: %v", err)
	}
	if !second.AlreadyMerged {
		t.Fatalf("повторная склейка должна вернуть AlreadyMerged: %+v", second)
	}
	var journal int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM entity_merges WHERE keep_id = $1`, w.Keep.ID).Scan(&journal); err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	if journal != 1 {
		t.Fatalf("журнал склейки должен быть один (id %d), строк %d", first.JournalID, journal)
	}
	if !equalStrings(aliasesBefore, aliasState(t, ctx, pool, w.Keep.ID)) {
		t.Fatal("повтор не должен задваивать алиасы")
	}
	if !equalStrings(linksBefore, linkState(t, ctx, pool, w.Keep.ID)) {
		t.Fatal("повтор не должен задваивать связи памяти")
	}
}

// TestUnmergeRestoresState — откат по журналу возвращает алиасы, связи,
// stances с основаниями и отношения ровно как до склейки; повторный откат —
// «не найдено».
func TestUnmergeRestoresState(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	scope := "chat:508"
	w := seedMergeWorld(t, ctx, pool, scope)
	repo := EntityRepo{}

	before := struct {
		keepAliases, mergeAliases []string
		keepLinks, mergeLinks     []string
		keepStances, mergeStances []string
		keepRels, mergeRels       []string
	}{
		keepAliases:  aliasState(t, ctx, pool, w.Keep.ID),
		mergeAliases: aliasState(t, ctx, pool, w.Merge.ID),
		keepLinks:    linkState(t, ctx, pool, w.Keep.ID),
		mergeLinks:   linkState(t, ctx, pool, w.Merge.ID),
		keepStances:  stanceState(t, ctx, pool, w.Keep.ID),
		mergeStances: stanceState(t, ctx, pool, w.Merge.ID),
		keepRels:     relationshipState(t, ctx, pool, w.Keep.ID),
		mergeRels:    relationshipState(t, ctx, pool, w.Merge.ID),
	}

	if _, err := repo.Merge(ctx, pool, EntityMergeRequest{
		KeepID: w.Keep.ID, MergeID: w.Merge.ID,
		Reason: "одно лицо", ProposedBy: "consolidation", Audience: scope,
	}); err != nil {
		t.Fatalf("склейка: %v", err)
	}

	res, err := repo.Unmerge(ctx, pool, w.Merge.ID)
	if err != nil {
		t.Fatalf("откат не выполнен: %v", err)
	}
	if res.KeepID != w.Keep.ID || res.MergeID != w.Merge.ID {
		t.Fatalf("итог отката: %+v", res)
	}
	if got := mergedInto(t, ctx, pool, w.Merge.ID); got != nil {
		t.Fatalf("после отката merged_into должен быть пуст: %v", got)
	}

	if !equalStrings(before.keepAliases, aliasState(t, ctx, pool, w.Keep.ID)) {
		t.Fatalf("алиасы keep после отката: %v, было %v", aliasState(t, ctx, pool, w.Keep.ID), before.keepAliases)
	}
	if !equalStrings(before.mergeAliases, aliasState(t, ctx, pool, w.Merge.ID)) {
		t.Fatalf("алиасы merge после отката: %v, было %v", aliasState(t, ctx, pool, w.Merge.ID), before.mergeAliases)
	}
	if !equalStrings(before.keepLinks, linkState(t, ctx, pool, w.Keep.ID)) {
		t.Fatalf("связи keep после отката: %v, было %v", linkState(t, ctx, pool, w.Keep.ID), before.keepLinks)
	}
	if !equalStrings(before.mergeLinks, linkState(t, ctx, pool, w.Merge.ID)) {
		t.Fatalf("связи merge после отката: %v, было %v", linkState(t, ctx, pool, w.Merge.ID), before.mergeLinks)
	}
	if !equalStrings(before.keepStances, stanceState(t, ctx, pool, w.Keep.ID)) {
		t.Fatalf("stances keep после отката: %v, было %v", stanceState(t, ctx, pool, w.Keep.ID), before.keepStances)
	}
	if !equalStrings(before.mergeStances, stanceState(t, ctx, pool, w.Merge.ID)) {
		t.Fatalf("stances merge после отката: %v, было %v", stanceState(t, ctx, pool, w.Merge.ID), before.mergeStances)
	}
	if !equalStrings(before.keepRels, relationshipState(t, ctx, pool, w.Keep.ID)) {
		t.Fatalf("отношения keep после отката: %v, было %v", relationshipState(t, ctx, pool, w.Keep.ID), before.keepRels)
	}
	if !equalStrings(before.mergeRels, relationshipState(t, ctx, pool, w.Merge.ID)) {
		t.Fatalf("отношения merge после отката: %v, было %v", relationshipState(t, ctx, pool, w.Merge.ID), before.mergeRels)
	}

	var rolledBack *string
	if err := pool.QueryRow(ctx,
		`SELECT rolled_back_at::text FROM entity_merges WHERE merge_id = $1`, w.Merge.ID).Scan(&rolledBack); err != nil {
		t.Fatalf("журнал после отката не читается: %v", err)
	}
	if rolledBack == nil {
		t.Fatal("журнал должен помнить время отката")
	}

	if _, err := repo.Unmerge(ctx, pool, w.Merge.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("повторный откат должен быть «не найдено»: %v", err)
	}
}
