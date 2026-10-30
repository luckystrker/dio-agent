// Retrieval-инструменты памяти (спека §10.4): только чтение; модель сама
// решает, что и когда спросить у памяти. Все три инструмента фильтруют
// приватность ДО выдачи (§9.2): фильтр в SQL, до извлечения текста и до
// семантического ранжирования; невидимые записи не отличимы от
// несуществующих.
//
// На прямое обращение (§5.2 приоритет 1) первый вызов инструмента памяти
// принудительный: forced tool-calling провайдера — гарантия «прямой вопрос
// всегда получает нужную память» держится механикой, а не дисциплиной
// модели (философия №8). Этот модуль только объявляет флаг
// (ForcedOnDirectQuestion на memory_search); проводку tool_choice на
// конкретном провайдере делает модуль хода (этап 9.4).
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	agotools "github.com/luckystrker/ago/tools"
)

// Имена инструментов памяти и ключ DI аудитории.
const (
	ToolFindEntities = "memory_find_entities"
	ToolEntityCard   = "memory_entity_card"
	ToolSearch       = "memory_search"

	// ScopeDependency — ключ зависимости в контексте хода: аудитория
	// (scope) текущего чата. Ход выставляет её до регистрации
	// инструментов; отсутствующая зависимость честно валит вызов —
	// память без аудитории не выдаётся (§9.2).
	ScopeDependency = "memory_scope"
)

// Лимиты выдачи инструментов.
const (
	toolSearchDefaultLimit = 10
	toolSearchMaxLimit     = 50
	cardMemoriesLimit      = 10
	// пул кандидатов карточки шире лимита: срез делается после фактора
	// «разнообразие оснований» (§10.4 п.5), иначе лимит забирает серия
	// записей одного источника
	cardMemoriesPool = 3 * cardMemoriesLimit
)

// ForcedOnDirectQuestion — маркер инструмента, чей вызов на прямом обращении
// принудителен (§10.4). Модуль хода находит такой инструмент среди
// зарегистрированных через type assertion и передаёт его имя в tool_choice.
type ForcedOnDirectQuestion interface {
	agotools.Tool
	ForceOnDirectQuestion() bool
}

// forcedTool оборачивает FunctionTool маркером принудительного вызова.
type forcedTool struct{ agotools.Tool }

func (forcedTool) ForceOnDirectQuestion() bool { return true }

// Toolset — retrieval-инструменты памяти. Репозитории без состояния,
// Searcher/Embedder безопасны для параллельных вызовов; чтения идут мимо
// транзакций (запись памяти — путь runtime, не инструментов).
type Toolset struct {
	db       DBTX
	entities EntityRepo
	items    Items
	stances  Stances
	search   *Searcher

	findEntitiesTool agotools.Tool
	entityCardTool   agotools.Tool
	searchTool       agotools.Tool
}

// NewToolset строит инструментарий чтения памяти. searcher без Embedder
// допустим: memory_search работает полнотекстом и сообщает о выключенной
// семантике.
func NewToolset(db DBTX, searcher *Searcher) (*Toolset, error) {
	if db == nil {
		return nil, errors.New("memory: Toolset без доступа к БД не существует")
	}
	if searcher == nil {
		return nil, errors.New("memory: Toolset без Searcher не существует")
	}

	t := &Toolset{db: db, search: searcher, items: Items{}}

	find, err := agotools.NewFunctionTool(ToolFindEntities,
		"Найти сущности памяти по имени или алиасу. Верни id найденной сущности, чтобы запросить её карточку.",
		func(args struct {
			Query string `json:"query" agschema:"description=Имя или алиас: как звучит в разговоре"`
			Scope string `aginject:"memory_scope"`
		}) (string, error) {
			return t.findEntities(context.Background(), args.Scope, args.Query)
		})
	if err != nil {
		return nil, fmt.Errorf("memory: инструмент %s: %w", ToolFindEntities, err)
	}

	card, err := agotools.NewFunctionTool(ToolEntityCard,
		"Карточка сущности памяти: текущие отношения (stances) с основаниями и связанные записи. Сначала найди сущность через "+ToolFindEntities+".",
		func(args struct {
			EntityID string `json:"entity_id" agschema:"description=Идентификатор из memory_find_entities"`
			Scope    string `aginject:"memory_scope"`
		}) (string, error) {
			return t.entityCard(context.Background(), args.Scope, args.EntityID)
		})
	if err != nil {
		return nil, fmt.Errorf("memory: инструмент %s: %w", ToolEntityCard, err)
	}

	search, err := agotools.NewFunctionTool(ToolSearch,
		"Полнотекстовый и семантический поиск по памяти: факты, события, мнения. На прямой вопрос о памяти вызывается первым.",
		func(args struct {
			Query string `json:"query" agschema:"description=Поисковый запрос обычными словами"`
			Limit *int   `json:"limit,omitempty" agschema:"description=Сколько записей вернуть (по умолчанию 10 максимум 50)"`
			Scope string `aginject:"memory_scope"`
		}) (string, error) {
			return t.searchMemory(context.Background(), args.Scope, args.Query, args.Limit)
		}, agotools.WithTimeout(defaultToolTimeout))
	if err != nil {
		return nil, fmt.Errorf("memory: инструмент %s: %w", ToolSearch, err)
	}

	t.findEntitiesTool = find
	t.entityCardTool = card
	t.searchTool = forcedTool{search}
	return t, nil
}

// defaultToolTimeout — потолок одного вызова поиска (векторизация запроса
// может ходить в сеть); срыв отвечает модели ToolErrorEvent.
const defaultToolTimeout = 30 * time.Second

// Tools возвращает инструменты для agent.WithTools.
func (t *Toolset) Tools() []agotools.Tool {
	return []agotools.Tool{t.findEntitiesTool, t.entityCardTool, t.searchTool}
}

// SearchTool возвращает инструмент текстового поиска — forced на прямом
// обращении (§10.4); модуль хода использует его имя для tool_choice.
func (t *Toolset) SearchTool() ForcedOnDirectQuestion {
	return t.searchTool.(ForcedOnDirectQuestion)
}

// findEntities — реализация memory_find_entities: поиск сущностей по имени
// и алиасам для аудитории.
func (t *Toolset) findEntities(ctx context.Context, audience, query string) (string, error) {
	if !IsValidScope(audience) {
		return "", fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	found, err := t.entities.ResolveByName(ctx, t.db, query, audience)
	if err != nil {
		return "", fmt.Errorf("memory_find_entities: %w", err)
	}
	out := make([]entityJSON, 0, len(found))
	for _, e := range found {
		out = append(out, entityJSON{ID: strconv.FormatInt(e.ID, 10), Kind: e.Kind, Name: e.CanonicalName, Scope: e.ScopeID})
	}
	return marshalJSON(entitiesResultJSON{Entities: out})
}

// entityCard — реализация memory_entity_card: карточка собирается сразу для
// конкретной аудитории (§9.2): сущность, её stances с видимыми основаниями и
// связанные записи. Скрытые основания считаются (hidden_basis), но их текст
// не выдаётся никогда.
func (t *Toolset) entityCard(ctx context.Context, audience, entityID string) (string, error) {
	if !IsValidScope(audience) {
		return "", fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	id, err := strconv.ParseInt(entityID, 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("memory_entity_card: entity_id %q не число", entityID)
	}

	entity, err := t.entities.Get(ctx, t.db, id, audience)
	if errors.Is(err, ErrNotFound) {
		// Существование невидимой сущности не раскрывается (§9.2).
		return "", fmt.Errorf("memory_entity_card: сущность %q не найдена", entityID)
	}
	if err != nil {
		return "", fmt.Errorf("memory_entity_card: %w", err)
	}

	stances, err := t.stances.ForEntities(ctx, t.db, audience, []int64{id})
	if err != nil {
		return "", fmt.Errorf("memory_entity_card: stances: %w", err)
	}
	stanceBasis := make(map[int64]basisWithHidden, len(stances))

	// Все видимые записи карточки (связанные + основания stances) собираются
	// в одну пачку: источники грузятся одним запросом и попадают в выдачу
	// (§10.4 п.6: компактная карточка с источниками).
	var allItems []Item
	for _, st := range stances {
		basis, hidden, err := t.stances.Basis(ctx, t.db, t.items, audience, st.ID)
		if err != nil {
			return "", fmt.Errorf("memory_entity_card: основания stance %d: %w", st.ID, err)
		}
		allItems = append(allItems, basis...)
		stanceBasis[st.ID] = basisWithHidden{basis: basis, hidden: hidden}
	}
	memories, err := t.items.ItemsForEntities(ctx, t.db, audience, []int64{id}, cardMemoriesPool)
	if err != nil {
		return "", fmt.Errorf("memory_entity_card: записи: %w", err)
	}
	// §10.4 п.5: разнообразие оснований до среза лимита карточки.
	memCites, err := t.items.SourcesFor(ctx, t.db, itemIDs(memories))
	if err != nil {
		return "", fmt.Errorf("memory_entity_card: источники записей: %w", err)
	}
	ordered := make([]Item, 0, len(memories))
	for _, i := range diverseOrder(memories, memCites) {
		ordered = append(ordered, memories[i])
	}
	if len(ordered) > cardMemoriesLimit {
		ordered = ordered[:cardMemoriesLimit]
	}
	allItems = append(allItems, ordered...)

	cites, err := t.items.SourcesFor(ctx, t.db, itemIDs(allItems))
	if err != nil {
		return "", fmt.Errorf("memory_entity_card: источники: %w", err)
	}

	stanceJSONs := make([]stanceJSON, 0, len(stances))
	for _, st := range stances {
		part := stanceBasis[st.ID]
		basisJSONs := make([]basisJSON, 0, len(part.basis))
		for _, b := range part.basis {
			basisJSONs = append(basisJSONs, basisJSON{
				Text:    b.Text,
				Sources: citeJSON(cites[b.ID]),
			})
		}
		stanceJSONs = append(stanceJSONs, stanceJSON{
			Aspect:      st.Aspect,
			Summary:     st.Summary,
			Strength:    st.Strength,
			Interest:    st.Interest,
			Revision:    st.Revision,
			Basis:       basisJSONs,
			HiddenBasis: part.hidden,
		})
	}

	memoryJSONs := make([]itemJSON, 0, len(ordered))
	for _, m := range ordered {
		memoryJSONs = append(memoryJSONs, itemToJSON(m, -1, cites[m.ID]))
	}

	return marshalJSON(cardResultJSON{
		Entity:   entityJSON{ID: strconv.FormatInt(entity.ID, 10), Kind: entity.Kind, Name: entity.CanonicalName, Scope: entity.ScopeID},
		Stances:  stanceJSONs,
		Memories: memoryJSONs,
	})
}

// searchMemory — реализация memory_search: full-text всегда, семантика при
// настроенном векторизаторе; о выключенной семантике ответ сообщает честно.
func (t *Toolset) searchMemory(ctx context.Context, audience, query string, limit *int) (string, error) {
	if !IsValidScope(audience) {
		return "", fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	size := toolSearchDefaultLimit
	if limit != nil {
		size = *limit
	}
	if size <= 0 || size > toolSearchMaxLimit {
		return "", fmt.Errorf("%w: memory_search: limit %d вне 1..%d", ErrInvalidCandidate, size, toolSearchMaxLimit)
	}

	results, err := t.search.Search(ctx, t.db, audience, query, size)
	if err != nil {
		return "", fmt.Errorf("memory_search: %w", err)
	}
	// Источники в выдаче — §10.4 п.6 (цитируемое основание каждой записи).
	cites, err := t.items.SourcesFor(ctx, t.db, itemIDs(scoredAsItems(results)))
	if err != nil {
		return "", fmt.Errorf("memory_search: источники: %w", err)
	}
	items := make([]itemJSON, 0, len(results))
	semanticUsed := false
	for _, r := range results {
		if r.Distance >= 0 {
			semanticUsed = true
		}
		items = append(items, itemToJSON(r.Item, r.Distance, cites[r.ID]))
	}
	return marshalJSON(searchResultJSON{Items: items, Semantic: semanticUsed})
}

// --- JSON-формы выдачи модели (имена полей — контракт для модели) ------------

type sourceJSON struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

type entityJSON struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

type itemJSON struct {
	ID       string       `json:"id"`
	Kind     string       `json:"kind"`
	Text     string       `json:"text"`
	Scope    string       `json:"scope"`
	Status   string       `json:"status"`
	Salience float64      `json:"salience"`
	Recorded string       `json:"recorded_at"`
	Sources  []sourceJSON `json:"sources"`
	Distance *float64     `json:"distance,omitempty"` // семантическая дистанция; отсутствует у full-text попаданий
}

type basisJSON struct {
	Text    string       `json:"text"`
	Sources []sourceJSON `json:"sources"`
}

type stanceJSON struct {
	Aspect      string      `json:"aspect"`
	Summary     string      `json:"summary"`
	Strength    float64     `json:"strength"`
	Interest    float64     `json:"interest"`
	Revision    int         `json:"revision"`
	Basis       []basisJSON `json:"basis"`
	HiddenBasis int         `json:"hidden_basis"`
}

// basisWithHidden — основания stance и число скрытых (не видимых аудитории),
// чтобы ответ мог честно сказать «часть оснований недоступна».
type basisWithHidden struct {
	basis  []Item
	hidden int
}

type entitiesResultJSON struct {
	Entities []entityJSON `json:"entities"`
}

type cardResultJSON struct {
	Entity   entityJSON   `json:"entity"`
	Stances  []stanceJSON `json:"stances"`
	Memories []itemJSON   `json:"memories"`
}

type searchResultJSON struct {
	Items    []itemJSON `json:"items"`
	Semantic bool       `json:"semantic"`
}

// citeJSON рендерит цитаты источников; пустой список — пустой массив, чтобы
// модель видела отсутствие источника явно.
func citeJSON(cites []SourceCite) []sourceJSON {
	out := make([]sourceJSON, 0, len(cites))
	for _, c := range cites {
		out = append(out, sourceJSON{Kind: c.Kind, Ref: c.Ref})
	}
	return out
}

func itemToJSON(m Item, distance float64, cites []SourceCite) itemJSON {
	out := itemJSON{
		ID:       strconv.FormatInt(m.ID, 10),
		Kind:     m.Kind,
		Text:     m.Text,
		Scope:    m.ScopeID,
		Status:   m.Status,
		Salience: m.Salience,
		Recorded: m.RecordedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Sources:  citeJSON(cites),
	}
	if distance >= 0 {
		d := distance
		out.Distance = &d
	}
	return out
}

func marshalJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("memory: сериализация результата инструмента: %w", err)
	}
	return string(raw), nil
}
