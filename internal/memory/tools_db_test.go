//go:build db

// БД-тесты retrieval-инструментов (этап 9.3): фильтр приватности §9.2
// срабатывает ДО выдачи — проверяется на уровне вызова инструмента, включая
// полный провод через механику ago (Register + DI аудитории +
// ToolCallEvent), а не только через репозиторий. Приватное из лички не
// всплывает в группе.
package memory

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agostream "github.com/luckystrker/ago/stream"
)

type toolResultJSON struct {
	Entities []entityJSON `json:"entities"`
	Items    []itemJSON   `json:"items"`
	Semantic bool         `json:"semantic"`
	Card     struct {
		Entity  entityJSON   `json:"entity"`
		Stances []stanceJSON `json:"stances"`
	} `json:"card"`
}

func newMemoryFixture(t *testing.T, ctx context.Context, pool DBTX) *Toolset {
	t.Helper()
	seedItem(t, ctx, pool, scopeA, "Личный секрет: пароль от сейфа в шкатулке", seedOpts{salience: 0.9})
	seedItem(t, ctx, pool, ScopePublic, "Публичный факт: музей старинной шкатулки открылся", seedOpts{salience: 0.5})
	seedItem(t, ctx, pool, scopeA, "Секретный проект метеор запускается в марте", seedOpts{
		links: []EntityLink{{Name: "Метеор", Kind: EntityProject, Role: RoleSubject}},
	})
	seedItem(t, ctx, pool, ScopePublic, "Публичный праздник Коляда идёт по площади", seedOpts{
		links: []EntityLink{{Name: "Коляда", Kind: EntityTopic, Role: RoleSubject}},
	})
	ts, err := NewToolset(pool, &Searcher{Embedder: nil, Items: Items{}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	return ts
}

func TestToolScopeIsolation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ts := newMemoryFixture(t, ctx, pool)

	t.Run("memory_search: личный секрет не всплывает в чужом чате", func(t *testing.T) {
		fromA, err := ts.searchMemory(ctx, scopeA, "шкатулка", nil)
		if err != nil {
			t.Fatalf("searchMemory(A): %v", err)
		}
		if !strings.Contains(fromA, "пароль от сейфа") {
			t.Errorf("свой чат не получил приватную запись: %s", fromA)
		}
		if !strings.Contains(fromA, `"semantic":false`) {
			t.Errorf("выключенная семантика должна быть честно помечена: %s", fromA)
		}

		fromB, err := ts.searchMemory(ctx, scopeB, "шкатулка", nil)
		if err != nil {
			t.Fatalf("searchMemory(B): %v", err)
		}
		var parsed toolResultJSON
		if err := json.Unmarshal([]byte(fromB), &parsed); err != nil {
			t.Fatalf("выдача не разбирается: %v: %s", err, fromB)
		}
		for _, it := range parsed.Items {
			if it.Scope != ScopePublic {
				t.Errorf("чужому чату утекло scope %q: %+v", it.Scope, it)
			}
			if strings.Contains(it.Text, "пароль от сейфа") {
				t.Errorf("личный секрет из лички утек в группу: %s", it.Text)
			}
		}
		if len(parsed.Items) == 0 {
			t.Error("public-записи должны были найтись и из чужого чата")
		}
	})

	t.Run("memory_find_entities: приватная сущность не находится из чужого чата", func(t *testing.T) {
		fromA, err := ts.findEntities(ctx, scopeA, "Метеор")
		if err != nil {
			t.Fatalf("findEntities(A): %v", err)
		}
		if !strings.Contains(fromA, "Метеор") {
			t.Errorf("свой чат не нашёл свою сущность: %s", fromA)
		}
		fromB, err := ts.findEntities(ctx, scopeB, "Метеор")
		if err != nil {
			t.Fatalf("findEntities(B): %v", err)
		}
		if strings.Contains(fromB, "Метеор") {
			t.Errorf("приватная сущность утекла в чужой чат: %s", fromB)
		}
		public, err := ts.findEntities(ctx, scopeB, "Коляда")
		if err != nil || !strings.Contains(public, "Коляда") {
			t.Errorf("публичная сущность не нашлась из чужого чата: %s, %v", public, err)
		}
	})

	t.Run("memory_entity_card: невидимая сущность неотличима от несуществующей", func(t *testing.T) {
		var meteorID string
		if err := pool.QueryRow(ctx,
			"SELECT id::text FROM entities WHERE canonical_name = 'Метеор'").Scan(&meteorID); err != nil {
			t.Fatalf("сущность не найдена: %v", err)
		}
		card, err := ts.entityCard(ctx, scopeA, meteorID)
		if err != nil {
			t.Fatalf("entityCard(A): %v", err)
		}
		if !strings.Contains(card, "Метеор") {
			t.Errorf("карточка без сущности: %s", card)
		}
		if _, err := ts.entityCard(ctx, scopeB, meteorID); err == nil {
			t.Error("чужой чат получил карточку приватной сущности — существование раскрыто")
		}
	})
}

func TestToolEntityCardStancesAndBasis(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ts, err := NewToolset(pool, &Searcher{Embedder: nil, Items: Items{}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	stances := Stances{}

	// Сущность появляется через связь записи (как из разговора) — одна,
	// та же, на которую вешается stance.
	linked := seedItem(t, ctx, pool, scopeA, "Гуляли на Коляду до утра", seedOpts{
		links: []EntityLink{{Name: "Коляда", Kind: EntityTopic, Role: RoleSubject}},
	})
	publicLinked := seedItem(t, ctx, pool, ScopePublic, "Коляда идёт по площади", seedOpts{
		links: []EntityLink{{Name: "Коляда", Kind: EntityTopic, Role: RoleSubject}},
	})
	var entity int64
	if err := pool.QueryRow(ctx,
		"SELECT entity_id FROM memory_links WHERE memory_id = $1", linked.ID).Scan(&entity); err != nil {
		t.Fatalf("связь не найдена: %v", err)
	}
	// Связи создаются в скоупе записи, поэтому приватная запись по умолчанию
	// получает свою сущность. Для карточки одной сущности обе зоны вешаем
	// на public-сущность напрямую (обслуживающая операция).
	var publicEntity int64
	if err := pool.QueryRow(ctx,
		"SELECT entity_id FROM memory_links WHERE memory_id = $1", publicLinked.ID).Scan(&publicEntity); err != nil {
		t.Fatalf("public-связь не найдена: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO memory_links (memory_id, entity_id, role) VALUES ($1, $2, 'subject') ON CONFLICT DO NOTHING",
		linked.ID, publicEntity); err != nil {
		t.Fatalf("связь приватной записи с public-сущностью: %v", err)
	}
	entity = publicEntity
	m1 := seedItem(t, ctx, pool, scopeA, "Основание: гуляли до утра", seedOpts{})
	if _, err := stances.Upsert(ctx, pool, StanceInput{
		EntityID: entity, Aspect: "atmosphere", ScopeID: scopeA,
		Summary: "атмосфера народных гуляний нравится", Strength: 0.7, Interest: 0.6,
		BasisMemoryIDs: []int64{m1.ID},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	card, err := ts.entityCard(ctx, scopeA, strconv.FormatInt(entity, 10))
	if err != nil {
		t.Fatalf("entityCard: %v", err)
	}
	var parsed struct {
		Entity   entityJSON   `json:"entity"`
		Stances  []stanceJSON `json:"stances"`
		Memories []itemJSON   `json:"memories"`
	}
	if err := json.Unmarshal([]byte(card), &parsed); err != nil {
		t.Fatalf("карточка не разбирается: %v: %s", err, card)
	}
	if parsed.Entity.Name != "Коляда" {
		t.Errorf("entity = %+v", parsed.Entity)
	}
	if len(parsed.Stances) != 1 || parsed.Stances[0].Summary != "атмосфера народных гуляний нравится" {
		t.Fatalf("stances = %+v", parsed.Stances)
	}
	stance := parsed.Stances[0]
	if len(stance.Basis) != 1 || !strings.Contains(stance.Basis[0].Text, "гуляли до утра") {
		t.Errorf("основания = %+v, хочу видимое основание", stance.Basis)
	}
	// §10.4 п.6: основания в карточке — с источниками
	if len(stance.Basis[0].Sources) == 0 || stance.Basis[0].Sources[0].Kind != SourceJournalEvent {
		t.Errorf("у основания нет источника: %+v", stance.Basis[0])
	}
	if stance.HiddenBasis != 0 {
		t.Errorf("hidden_basis = %d, хочу 0", stance.HiddenBasis)
	}
	// связанные записи обеих зон: приватная и public
	if len(parsed.Memories) != 2 {
		t.Fatalf("связанных записей %d, хочу 2", len(parsed.Memories))
	}
	for _, m := range parsed.Memories {
		// §10.4 п.6: каждая запись карточки цитирует свои источники
		if len(m.Sources) == 0 || m.Sources[0].Kind != SourceJournalEvent || m.Sources[0].Ref == "" {
			t.Errorf("запись %q без источника: %+v", m.Text, m)
		}
	}
	ids := map[string]bool{}
	for _, m := range parsed.Memories {
		ids[m.ID] = true
	}
	if !ids[strconv.FormatInt(linked.ID, 10)] || !ids[strconv.FormatInt(publicLinked.ID, 10)] {
		t.Errorf("в карточке не хватает связанной записи: %+v", parsed.Memories)
	}
}

// TestToolThroughAgoMachinery — полный провод вызова через механику ago:
// инструмент зарегистрирован на потоке хода, аудитория приходит DI-зависи-
// мостью (как будет делать модуль хода 9.4), вызов — настоящий ToolCallEvent,
// результат — настоящий ToolResultEvent. Приватное из лички не всплывает
// в группе уже на этом уровне.
func TestToolThroughAgoMachinery(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ts := newMemoryFixture(t, ctx, pool)

	search := ts.SearchTool()
	if _, ok := search.(ForcedOnDirectQuestion); !ok {
		t.Fatal("memory_search потерял forced-маркер (§10.4)")
	}

	newTurn := func(audience string) (*agostream.MemoryStream, *agocontext.Context) {
		stream := agostream.NewMemoryStream()
		deps := map[string]any{}
		if audience != "" {
			deps[ScopeDependency] = audience
		}
		cx := agocontext.NewWithDeps(stream, deps, nil)
		subs, err := search.Register(cx, nil)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		t.Cleanup(func() {
			for _, id := range subs {
				stream.Unsubscribe(id)
			}
		})
		return stream, cx
	}

	run := func(t *testing.T, audience, args string) string {
		t.Helper()
		stream, cx := newTurn(audience)
		if err := stream.Send(events.ToolCallEvent{
			ID: "call-1", Name: ToolSearch, Arguments: args,
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		history, err := cx.History()
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(history) == 0 {
			t.Fatal("результат не доставлен")
		}
		last := events.AsValue(history[len(history)-1])
		result, ok := last.(events.ToolResultEvent)
		if !ok {
			t.Fatalf("последнее событие %T, хочу ToolResultEvent", last)
		}
		if result.Name != ToolSearch {
			t.Errorf("результат от %q, хочу %q", result.Name, ToolSearch)
		}
		if len(result.Result.Parts) != 1 {
			t.Fatalf("частей результата = %d, хочу 1", len(result.Result.Parts))
		}
		text, ok := result.Result.Parts[0].(*events.TextInput)
		if !ok {
			t.Fatalf("часть результата %T, хочу *events.TextInput", result.Result.Parts[0])
		}
		return text.Content
	}

	t.Run("группа не видит личный секрет", func(t *testing.T) {
		got := run(t, scopeB, `{"query":"шкатулка"}`)
		if strings.Contains(got, "пароль от сейфа") {
			t.Errorf("личный секрет утек в группу: %s", got)
		}
		if !strings.Contains(got, "музей старинной шкатулки") {
			t.Errorf("публичная запись не нашлась: %s", got)
		}
	})

	t.Run("личный чат видит свой секрет", func(t *testing.T) {
		got := run(t, scopeA, `{"query":"шкатулка"}`)
		if !strings.Contains(got, "пароль от сейфа") {
			t.Errorf("свой чат не получил свою запись: %s", got)
		}
	})

	t.Run("без DI-зависимости аудитории вызов падает честно", func(t *testing.T) {
		stream, cx := newTurn("")
		if err := stream.Send(events.ToolCallEvent{ID: "c2", Name: ToolSearch, Arguments: `{"query":"секрет"}`}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		history, err := cx.History()
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		last := events.AsValue(history[len(history)-1])
		toolErr, ok := last.(events.ToolErrorEvent)
		if !ok {
			t.Fatalf("последнее событие %T, хочу ToolErrorEvent", last)
		}
		if !strings.Contains(toolErr.Error, ScopeDependency) {
			t.Errorf("ошибка = %q, хочу упоминание %q", toolErr.Error, ScopeDependency)
		}
	})
}

// §10.4 п.5–6 в memory_search: записи цитируют источники; фактор
// «разнообразие оснований» применяется до среза limit — серия записей
// одного источника не забирает всю выдачу.
func TestToolSearchSourcesAndGroundDiversity(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ts, err := NewToolset(pool, &Searcher{Embedder: nil, Items: Items{}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}

	// Три свежие записи одного источника и одна старая с другим источником.
	prov, err := (SourceRepo{}).EnsureProvenance(ctx, pool, SourceJournalEvent, "prov:diversity", []string{scopeA})
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	items := Items{}
	shared := SourceRef{Kind: SourceJournalEvent, Ref: "turn:shared"}
	other := SourceRef{Kind: SourceJournalEvent, Ref: "turn:other"}
	conf := 0.5
	same := func(text string) Item {
		t.Helper()
		it, err := items.Insert(ctx, pool, NewItem{
			Kind: KindClaim, Text: text, Salience: 0.9, Confidence: &conf,
			ProvenanceID: prov, Source: &shared,
		}, scopeA)
		if err != nil {
			t.Fatalf("вставка %q: %v", text, err)
		}
		return it
	}
	same("Гулянка на площади началась в полдень")
	same("Гулянка продолжилась вечером")
	topRanked := same("Гулянка закончилась салютом") // равная salience: самая свежая — первая
	lessRanked, err := items.Insert(ctx, pool, NewItem{
		Kind: KindClaim, Text: "Гулянка в парке — отдельная история", Salience: 0.4, Confidence: &conf,
		ProvenanceID: prov, Source: &other,
	}, scopeA)
	if err != nil {
		t.Fatalf("вставка другой-источник: %v", err)
	}

	raw, err := ts.searchMemory(ctx, scopeA, "гулянка", &[]int{2}[0])
	if err != nil {
		t.Fatalf("searchMemory: %v", err)
	}
	var parsed toolResultJSON
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("выдача не разбирается: %v: %s", err, raw)
	}

	// §10.4 п.6: каждая запись цитирует источник
	if len(parsed.Items) != 2 {
		t.Fatalf("найдено %d, хочу 2 (limit)", len(parsed.Items))
	}
	if parsed.Items[0].ID != strconv.FormatInt(topRanked.ID, 10) {
		t.Errorf("первая запись = %s, хочу самую значимую %d", parsed.Items[0].ID, topRanked.ID)
	}
	for _, it := range parsed.Items {
		if len(it.Sources) == 0 || it.Sources[0].Kind != SourceJournalEvent || it.Sources[0].Ref == "" {
			t.Errorf("запись %q без источника: %+v", it.Text, it)
		}
	}
	// §10.4 п.5: второй слот отдан записи с другим источником, а не второй
	// записи того же источника
	if parsed.Items[1].ID != strconv.FormatInt(lessRanked.ID, 10) {
		t.Errorf("второй слот = %s (%q), хочу запись другого источника %d", parsed.Items[1].ID, parsed.Items[1].Text, lessRanked.ID)
	}
	if parsed.Items[1].Sources[0].Ref != "turn:other" {
		t.Errorf("источник второй записи = %+v, хочу turn:other", parsed.Items[1].Sources)
	}
}

// §10.4 п.5–6 в карточке: записи и основания цитируют источники; срез лимита
// карточки применяется после разнообразия оснований.
func TestToolCardGroundDiversity(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ts, err := NewToolset(pool, &Searcher{Embedder: nil, Items: Items{}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}

	prov, err := (SourceRepo{}).EnsureProvenance(ctx, pool, SourceJournalEvent, "prov:card-div", []string{scopeA})
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	entity := seedRawEntity(t, ctx, pool, EntityTopic, "Ярмарка", scopeA)
	items := Items{}
	shared := SourceRef{Kind: SourceJournalEvent, Ref: "turn:shared"}
	other := SourceRef{Kind: SourceJournalEvent, Ref: "turn:other"}
	conf := 0.5
	link := func(text string, salience float64, source SourceRef) Item {
		t.Helper()
		it, err := items.Insert(ctx, pool, NewItem{
			Kind: KindClaim, Text: text, Salience: salience, Confidence: &conf,
			ProvenanceID: prov, Source: &source,
			Links: []EntityLink{{Name: "Ярмарка", Kind: EntityTopic, Role: RoleSubject}},
		}, scopeA)
		if err != nil {
			t.Fatalf("вставка %q: %v", text, err)
		}
		return it
	}
	// вторая и третья по значимости — одного источника; вторая по порядку
	// выдачи обязана уступить место записи с другим источником
	link("Ярмарка открылась в полдень", 0.9, shared)
	third := link("Ярмарка закрылась салютом", 0.7, shared)
	second := link("Ярмарка в парке — отдельная история", 0.5, other)

	card, err := ts.entityCard(ctx, scopeA, strconv.FormatInt(entity, 10))
	if err != nil {
		t.Fatalf("entityCard: %v", err)
	}
	var parsed struct {
		Memories []itemJSON `json:"memories"`
	}
	if err := json.Unmarshal([]byte(card), &parsed); err != nil {
		t.Fatalf("карточка не разбирается: %v: %s", err, card)
	}
	if len(parsed.Memories) < 3 {
		t.Fatalf("записей в карточке %d, хочу 3", len(parsed.Memories))
	}
	if parsed.Memories[1].ID != strconv.FormatInt(second.ID, 10) {
		t.Errorf("вторая запись = %s, хочу другой источник %d", parsed.Memories[1].ID, second.ID)
	}
	if parsed.Memories[2].ID != strconv.FormatInt(third.ID, 10) {
		t.Errorf("третья запись = %s, хочу %d", parsed.Memories[2].ID, third.ID)
	}
	for _, m := range parsed.Memories {
		if len(m.Sources) == 0 || m.Sources[0].Ref == "" {
			t.Errorf("запись %q без источника: %+v", m.Text, m)
		}
	}
}
