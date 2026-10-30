// Юнит-тесты retrieval-инструментов (§10.4): схемы аргументов без DI-полей,
// forced-маркер только на memory_search, честный отказ без аудитории.
// Живые вызовы с БД — в *_db_test.go.
package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	agotools "github.com/luckystrker/ago/tools"
)

func newTestToolset(t *testing.T, emb Embedder) *Toolset {
	t.Helper()
	ts, err := NewToolset(failDB{}, &Searcher{Embedder: emb, Items: Items{}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	return ts
}

// failDB — заглушка DBTX: любой запрос — ошибка. Юнит-тесты не должны
// доходить до БД; дошедший вызов честно проваливается.
type failDB struct{}

func (failDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("failDB:unexpected db access")
}

func (failDB) Query(context.Context, string, ...any) (pgxRows, error) {
	return nil, errors.New("failDB:unexpected db access")
}

func (failDB) QueryRow(context.Context, string, ...any) pgxRow {
	return nil
}

func TestToolsetConstruction(t *testing.T) {
	ts := newTestToolset(t, nil)

	tools := ts.Tools()
	if len(tools) != 3 {
		t.Fatalf("инструментов = %d, хочу 3", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range ts.Tools() {
		schemas, err := tool.Schemas(nil)
		if err != nil {
			t.Fatalf("%s: Schemas: %v", tool.Name(), err)
		}
		if len(schemas) != 1 {
			t.Fatalf("%s: схем = %d, хочу 1", tool.Name(), len(schemas))
		}
		schema, ok := schemas[0].(agotools.Schema)
		if !ok {
			t.Fatalf("%s: схема не tools.Schema: %T", tool.Name(), schemas[0])
		}
		if schema.Name != tool.Name() {
			t.Errorf("имя схемы %q не совпало с инструментом %q", schema.Name, tool.Name())
		}
		if !tool.DeclaredInCode() {
			t.Errorf("%s: инструмент из кода должен быть DeclaredInCode", schema.Name)
		}
		names[schema.Name] = true

		// DI-поле аудитории не попадает в схему модели: аудитория назначается
		// ходом, а не предлагается моделью (§9.2).
		props, _ := schema.Parameters["properties"].(map[string]any)
		if _, di := props["scope"]; di {
			t.Errorf("%s: DI-поле scope просочилось в схему: %v", schema.Name, props)
		}
	}
	for _, want := range []string{ToolFindEntities, ToolEntityCard, ToolSearch} {
		if !names[want] {
			t.Errorf("нет инструмента %q, есть %v", want, names)
		}
	}
}

func TestToolsetSchemaArguments(t *testing.T) {
	ts := newTestToolset(t, nil)
	for _, tool := range ts.Tools() {
		schemas, _ := tool.Schemas(nil)
		schema := schemas[0].(agotools.Schema)
		props, _ := schema.Parameters["properties"].(map[string]any)

		switch tool.Name() {
		case ToolFindEntities:
			if _, ok := props["query"]; !ok {
				t.Errorf("%s: нет аргумента query: %v", tool.Name(), props)
			}
		case ToolEntityCard:
			if _, ok := props["entity_id"]; !ok {
				t.Errorf("%s: нет аргумента entity_id: %v", tool.Name(), props)
			}
		case ToolSearch:
			if _, ok := props["query"]; !ok {
				t.Errorf("%s: нет аргумента query: %v", tool.Name(), props)
			}
			if _, ok := props["limit"]; !ok {
				t.Errorf("%s: нет необязательного limit: %v", tool.Name(), props)
			}
		}
	}
}

func TestForcedMarkerOnlyOnSearch(t *testing.T) {
	ts := newTestToolset(t, nil)
	for _, tool := range ts.Tools() {
		forced, ok := tool.(ForcedOnDirectQuestion)
		if tool.Name() == ToolSearch {
			if !ok || !forced.ForceOnDirectQuestion() {
				t.Fatalf("%s должен нести forced-маркер (§10.4)", ToolSearch)
			}
			continue
		}
		if ok {
			t.Errorf("%s не должен нести forced-маркер", tool.Name())
		}
	}
	if got := ts.SearchTool().Name(); got != ToolSearch {
		t.Errorf("SearchTool().Name() = %q, хочу %q", got, ToolSearch)
	}
}

func TestToolRejectsMissingOrInvalidAudience(t *testing.T) {
	ts := newTestToolset(t, nil)

	// Память без аудитории не выдаётся: отсутствие DI-зависимости и мусор в
	// ней — явный отказ, до любого похода в БД (failDB упал бы иначе).
	for _, audience := range []string{"", "group", "chat:abc"} {
		if _, err := ts.findEntities(context.Background(), audience, "Новый год"); !errors.Is(err, ErrInvalidScope) {
			t.Errorf("findEntities(%q): err = %v, хочу ErrInvalidScope", audience, err)
		}
		if _, err := ts.entityCard(context.Background(), audience, "1"); !errors.Is(err, ErrInvalidScope) {
			t.Errorf("entityCard(%q): err = %v, хочу ErrInvalidScope", audience, err)
		}
		if _, err := ts.searchMemory(context.Background(), audience, "праздник", nil); !errors.Is(err, ErrInvalidScope) {
			t.Errorf("searchMemory(%q): err = %v, хочу ErrInvalidScope", audience, err)
		}
	}
}

func TestSearchMemoryValidatesArgs(t *testing.T) {
	ts := newTestToolset(t, nil)

	if _, err := ts.searchMemory(context.Background(), "chat:1", "", nil); !errors.Is(err, ErrInvalidCandidate) {
		t.Errorf("пустой запрос: err = %v, хочу ErrInvalidCandidate", err)
	}
	bad := 0
	if _, err := ts.searchMemory(context.Background(), "chat:1", "праздник", &bad); err == nil || !errors.Is(err, ErrInvalidCandidate) {
		t.Errorf("limit=0: err = %v, хочу ErrInvalidCandidate", err)
	}
	tooBig := toolSearchMaxLimit + 1
	if _, err := ts.searchMemory(context.Background(), "chat:1", "праздник", &tooBig); !errors.Is(err, ErrInvalidCandidate) {
		t.Errorf("limit>%d: err = %v, хочу ErrInvalidCandidate", toolSearchMaxLimit, err)
	}
}

func TestEntityCardRejectsBadID(t *testing.T) {
	ts := newTestToolset(t, nil)
	for _, bad := range []string{"", "abc", "0", "-3"} {
		if _, err := ts.entityCard(context.Background(), "chat:1", bad); err == nil || errors.Is(err, ErrInvalidScope) {
			t.Errorf("entityCard(id=%q): err = %v, ожидаю отказ по формату id", bad, err)
		}
	}
}

// §10.4 п.5: разнообразие оснований — стабильный жадный проход по источникам.
func TestDiverseOrder(t *testing.T) {
	items := []Item{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}}
	sources := map[int64][]SourceCite{
		1: {{Kind: SourceJournalEvent, Ref: "turn:1"}},
		2: {{Kind: SourceJournalEvent, Ref: "turn:1"}}, // тот же источник, что у 1
		3: {{Kind: SourceJournalEvent, Ref: "turn:2"}},
		// 4 без источников — общая группа «оснований нет»
	}

	got := diverseOrder(items, sources)
	want := []int{0, 2, 3, 1} // новые источники и «без источника» вперёд, повтор src1 — в хвост
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("diverseOrder = %v, хочу %v", got, want)
		}
	}

	// разные источники — исходный порядок ранжирования сохраняется
	distinct := map[int64][]SourceCite{
		1: {{Kind: SourceJournalEvent, Ref: "a"}},
		2: {{Kind: SourceJournalEvent, Ref: "b"}},
		3: {{Kind: SourceJournalEvent, Ref: "c"}},
		4: {{Kind: SourceJournalEvent, Ref: "d"}},
	}
	got = diverseOrder(items, distinct)
	for i := range got {
		if got[i] != i {
			t.Fatalf("при разных источниках порядок должен сохраниться, получено %v", got)
		}
	}

	// первый элемент всегда первый (доминирующая запись не отодвигается)
	same := map[int64][]SourceCite{
		1: {{Kind: SourceJournalEvent, Ref: "x"}},
		2: {{Kind: SourceJournalEvent, Ref: "x"}},
	}
	got = diverseOrder(items[:2], same)
	if got[0] != 0 {
		t.Fatalf("первый элемент = %d, хочу 0", got[0])
	}
}
