//go:build db

// БД-тесты stances и поиска (этап 9.3): актуальное отношение с основаниями
// (§8.4), полнотекстовый поиск по русской морфологии и ранжирование
// (паритет TS-справочника), семантический pgvector-поиск с фильтром до
// ранжирования (§9.2), индексация эмбеддингов.
package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRepoStances(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	stances := Stances{}
	items := Items{}

	entity := seedRawEntity(t, ctx, pool, EntityTopic, "новый год", ScopePublic)

	t.Run("upsert растит revision и дополняет основания, не стирая их", func(t *testing.T) {
		m1 := seedItem(t, ctx, pool, scopeA, "Основание: тихие домашние праздники", seedOpts{})
		m2 := seedItem(t, ctx, pool, scopeA, "Основание: суета утомляет", seedOpts{})

		first, err := stances.Upsert(ctx, pool, StanceInput{
			EntityID: entity, Aspect: "atmosphere", ScopeID: scopeA,
			Summary: "нравится уют дома", Strength: 0.7, Interest: 0.5,
			BasisMemoryIDs: []int64{m1.ID},
		})
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		second, err := stances.Upsert(ctx, pool, StanceInput{
			EntityID: entity, Aspect: "atmosphere", ScopeID: scopeA,
			Summary: "нравится уют дома и тишина", Strength: 0.8, Interest: 0.6,
			BasisMemoryIDs: []int64{m2.ID},
		})
		if err != nil {
			t.Fatalf("Upsert(2): %v", err)
		}
		if second.ID != first.ID {
			t.Errorf("upsert создал вторую строку %d вместо пересмотра %d", second.ID, first.ID)
		}
		if second.Revision != first.Revision+1 {
			t.Errorf("revision %d, хочу %d+1", second.Revision, first.Revision)
		}
		var basisCount int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM stance_basis WHERE stance_id = $1", first.ID).Scan(&basisCount); err != nil {
			t.Fatalf("основания не читаются: %v", err)
		}
		if basisCount != 2 {
			t.Errorf("оснований = %d, хочу 2 (прошлые не стираются)", basisCount)
		}
	})

	t.Run("видны stances своего scope и public; чужого и internal — нет (§9.2)", func(t *testing.T) {
		foreign := seedRawEntity(t, ctx, pool, EntityTopic, "море", scopeB)
		internal := seedRawEntity(t, ctx, pool, EntityTopic, "самоанализ", ScopeInternal)
		if _, err := stances.Upsert(ctx, pool, StanceInput{EntityID: entity, Aspect: "general", ScopeID: ScopePublic,
			Summary: "смешанное отношение к суете", Strength: 0.5, Interest: 0.4}); err != nil {
			t.Fatalf("Upsert(public): %v", err)
		}
		if _, err := stances.Upsert(ctx, pool, StanceInput{EntityID: foreign, Aspect: "general", ScopeID: scopeB,
			Summary: "чужое приватное мнение о море", Strength: 0.9, Interest: 0.9}); err != nil {
			t.Fatalf("Upsert(foreign): %v", err)
		}
		if _, err := stances.Upsert(ctx, pool, StanceInput{EntityID: internal, Aspect: "general", ScopeID: ScopeInternal,
			Summary: "внутренняя заметка персоны", Strength: 0.7, Interest: 0.2}); err != nil {
			t.Fatalf("Upsert(internal): %v", err)
		}

		visible, err := stances.ForEntities(ctx, pool, scopeA, []int64{entity, foreign, internal})
		if err != nil {
			t.Fatalf("ForEntities: %v", err)
		}
		// Свои (atmosphere chat:A из первого сабтеста) + public по своей
		// сущности; чужое море и internal-самоанализ не протекают.
		if len(visible) != 2 {
			t.Fatalf("видимых stances = %d, хочу 2 (свой + public)", len(visible))
		}
		for _, st := range visible {
			if st.EntityID == foreign || st.EntityID == internal {
				t.Errorf("чужая сущность протекла: %+v", st)
			}
			if strings.Contains(st.Summary, "чужое") || strings.Contains(st.Summary, "внутренняя") {
				t.Errorf("чужое резюме протекло: %q", st.Summary)
			}
		}
		// внутренний контекст видит всё: два stance своей сущности (свой
		// и public), чужое море, внутренний самоанализ — 4
		all, err := stances.ForEntities(ctx, pool, ScopeInternal, []int64{entity, foreign, internal})
		if err != nil {
			t.Fatalf("ForEntities(internal): %v", err)
		}
		if len(all) != 4 {
			t.Errorf("internal-аудитория видит %d, хочу 4", len(all))
		}
	})

	t.Run("приватный stance на публичную сущность не виден чужому чату", func(t *testing.T) {
		shared := seedRawEntity(t, ctx, pool, EntityTopic, "день города", ScopePublic)
		if _, err := stances.Upsert(ctx, pool, StanceInput{EntityID: shared, Aspect: "atmosphere", ScopeID: scopeA,
			Summary: "нравится салют над рекой", Strength: 0.6, Interest: 0.5}); err != nil {
			t.Fatalf("Upsert(A): %v", err)
		}
		if _, err := stances.Upsert(ctx, pool, StanceInput{EntityID: shared, Aspect: "atmosphere", ScopeID: scopeB,
			Summary: "не нравится шум", Strength: 0.6, Interest: 0.4}); err != nil {
			t.Fatalf("Upsert(B): %v", err)
		}
		fromA, err := stances.ForEntities(ctx, pool, scopeA, []int64{shared})
		if err != nil || len(fromA) != 1 || fromA[0].Summary != "нравится салют над рекой" {
			t.Errorf("из A = %v, %v", fromA, err)
		}
		fromB, err := stances.ForEntities(ctx, pool, scopeB, []int64{shared})
		if err != nil || len(fromB) != 1 || fromB[0].Summary != "не нравится шум" {
			t.Errorf("из B = %v, %v", fromB, err)
		}
	})

	t.Run("Basis выдаёт только видимые основания, скрытые считает (§9.2)", func(t *testing.T) {
		own := seedItem(t, ctx, pool, scopeA, "Видимое основание про салют", seedOpts{})
		hiddenProv := seedRawProvenance(t, ctx, pool, []string{scopeB})
		hiddenID := seedRawItem(t, ctx, pool, scopeB, "Чужое скрытое основание", "active", hiddenProv)
		st, err := stances.Upsert(ctx, pool, StanceInput{EntityID: entity, Aspect: "fireworks", ScopeID: scopeA,
			Summary: "салют нравится", Strength: 0.5, Interest: 0.5,
			BasisMemoryIDs: []int64{own.ID, hiddenID}})
		if err != nil {
			t.Fatalf("Upsert с чужим основанием: %v", err)
		}
		basis, hidden, err := stances.Basis(ctx, pool, items, scopeA, st.ID)
		if err != nil {
			t.Fatalf("Basis: %v", err)
		}
		if len(basis) != 1 || basis[0].Text != "Видимое основание про салют" {
			t.Errorf("видимые основания = %v, хочу только своё", texts(basis))
		}
		if hidden != 1 {
			t.Errorf("скрытых = %d, хочу 1", hidden)
		}
		// чужой чат по тому же stance: своё скрыто (скрытым стало), чужое — видимо
		basisB, hiddenB, err := stances.Basis(ctx, pool, items, scopeB, st.ID)
		if err != nil {
			t.Fatalf("Basis(B): %v", err)
		}
		if len(basisB) != 1 || basisB[0].Text != "Чужое скрытое основание" {
			t.Errorf("из B: видимые = %v, хочу только его собственное основание", texts(basisB))
		}
		if hiddenB != 1 {
			t.Errorf("из B: скрытых %d, хочу 1 (моя запись)", hiddenB)
		}
	})

	t.Run("неизвестная ссылка на память отклоняется (§10.1)", func(t *testing.T) {
		_, err := stances.Upsert(ctx, pool, StanceInput{EntityID: entity, Aspect: "broken", ScopeID: scopeA,
			Summary: "ссылка в никуда", Strength: 0.5, Interest: 0.5, BasisMemoryIDs: []int64{987654}})
		if err == nil {
			t.Error("основание с несуществующей памятью должно падать по FK")
		}
	})
}

// mapEmbedder — заглушка векторизатора с ручным словарём: тест задаёт
// векторы сам и проверяет семантический порядок независимо от формул.
type mapEmbedder struct {
	vectors map[string][]float32
}

func (m mapEmbedder) Model() string { return "map-stub-embedder" }

func (m mapEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		vec, ok := m.vectors[text]
		if !ok {
			return nil, fmt.Errorf("mapEmbedder: нет вектора для %q", text)
		}
		out[i] = vec
	}
	return out, nil
}

func TestRepoSemanticSearch(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	items := Items{}

	emb := mapEmbedder{vectors: map[string][]float32{
		"кошка":                 {1, 0, 0},
		"У соседей живёт кот":   {0.93, 0.36, 0},
		"Машина прошла ТО":      {0, 1, 0},
		"Кошка обещала котёнка": {1, 0, 0},
	}}

	// public: «У соседей живёт кот» близок к «кошка», машина — далеко.
	cat := seedItem(t, ctx, pool, ScopePublic, "У соседей живёт кот", seedOpts{salience: 0.5})
	car := seedItem(t, ctx, pool, ScopePublic, "Машина прошла ТО", seedOpts{salience: 0.9})
	// Приватная запись чата A ещё ближе к «кошка», чем «кот».
	kitten := seedItem(t, ctx, pool, scopeA, "Кошка обещала котёнка", seedOpts{salience: 0.9})
	_ = kitten

	indexed, err := IndexPending(ctx, pool, emb, items, 100)
	if err != nil {
		t.Fatalf("IndexPending: %v", err)
	}
	if indexed != 3 {
		t.Fatalf("проиндексировано %d, хочу 3", indexed)
	}

	t.Run("семантический порядок: близкая запись первой", func(t *testing.T) {
		vec := emb.vectors["кошка"]
		found, err := items.SearchSemantic(ctx, pool, ScopePublic, vec, emb.Model(), 10)
		if err != nil {
			t.Fatalf("SearchSemantic: %v", err)
		}
		if len(found) != 2 {
			t.Fatalf("public-аудитория видит %d записей, хочу 2", len(found))
		}
		if found[0].ID != cat.ID || found[1].ID != car.ID {
			t.Errorf("порядок = [%d %d], хочу кот раньше машины", found[0].ID, found[1].ID)
		}
		if found[0].Distance > found[1].Distance {
			t.Errorf("дистанции не возрастают: %v > %v", found[0].Distance, found[1].Distance)
		}
	})

	t.Run("фильтр ДО ранжирования: ближайшая приватная запись не выдаётся чужому чату", func(t *testing.T) {
		vec := emb.vectors["кошка"]
		found, err := items.SearchSemantic(ctx, pool, scopeB, vec, emb.Model(), 10)
		if err != nil {
			t.Fatalf("SearchSemantic(B): %v", err)
		}
		for _, s := range found {
			if s.ID == kitten.ID {
				t.Error("приватная запись, ближайшая к запросу, утекла в чужой чат")
			}
		}
		if len(found) != 2 {
			t.Errorf("из B видимо %d записей, хочу 2 public", len(found))
		}
		// свой чат видит все три, приватная — первой (ближе всех)
		fromA, err := items.SearchSemantic(ctx, pool, scopeA, vec, emb.Model(), 10)
		if err != nil {
			t.Fatalf("SearchSemantic(A): %v", err)
		}
		if len(fromA) != 3 || fromA[0].ID != kitten.ID {
			t.Errorf("из A порядок = %v, хочу приватную ближайшую первой", scoredIDs(fromA))
		}
	})

	t.Run("вектор другой модели не участвует", func(t *testing.T) {
		found, err := items.SearchSemantic(ctx, pool, ScopePublic, emb.vectors["кошка"], "other-model", 10)
		if err != nil {
			t.Fatalf("SearchSemantic: %v", err)
		}
		if len(found) != 0 {
			t.Errorf("чужая модель выдала %d записей, хочу 0", len(found))
		}
	})
}

func scoredIDs(items []ScoredItem) []int64 {
	out := make([]int64, 0, len(items))
	for _, s := range items {
		out = append(out, s.ID)
	}
	return out
}

func TestSearcherMergedAndSemanticOff(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	items := Items{}

	emb := mapEmbedder{vectors: map[string][]float32{
		"кофе":               {1, 0},
		"Кофе по утрам":      {0.99, 0.14},
		"Утренний ритуал":    {0.97, 0.26},
		"Сырники на завтрак": {0, 1},
		"Кофе без кофеина":   {0.99, 0.14},
	}}

	ftsHit := seedItem(t, ctx, pool, ScopePublic, "Кофе по утрам", seedOpts{salience: 0.9})
	semanticOnly := seedItem(t, ctx, pool, ScopePublic, "Утренний ритуал", seedOpts{salience: 0.9})
	seedItem(t, ctx, pool, ScopePublic, "Сырники на завтрак", seedOpts{salience: 0.9})
	seedItem(t, ctx, pool, scopeA, "Кофе без кофеина", seedOpts{salience: 0.5}) // виден только A
	if _, err := IndexPending(ctx, pool, emb, items, 100); err != nil {
		t.Fatalf("IndexPending: %v", err)
	}

	t.Run("объединение каналов: без дублей, full-text порядок сохраняется", func(t *testing.T) {
		searcher := &Searcher{Embedder: emb, Items: items}
		got, err := searcher.Search(ctx, pool, scopeA, "кофе", 10)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		// «Кофе по утрам» и «Кофе без кофеина» — full-text; «Кофе по утрам»
		// также попадает в семантику — в выдаче одна строка (не дубль);
		// «Утренний ритуал» и «Сырники» — семантические дополнения.
		if len(got) != 4 {
			t.Fatalf("найдено %d, хочу 4 без дублей: %v", len(got), got)
		}
		ids := scoredIDs(got)
		seen := map[int64]bool{}
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("дубль записи %d в выдаче", id)
			}
			seen[id] = true
		}
		// порядок полнотекстового канала: salience DESC, потом свежесть
		if got[0].ID != ftsHit.ID || got[0].Distance >= 0 {
			t.Errorf("первый результат = %d (distance %v), хочу full-text «Кофе по утрам» без дистанции", got[0].ID, got[0].Distance)
		}
		foundRitual := false
		for _, s := range got[1:] {
			if s.ID == semanticOnly.ID && s.Distance >= 0 {
				foundRitual = true
			}
		}
		if !foundRitual {
			t.Error("семантическое дополнение «Утренний ритуал» не найдено")
		}
	})

	t.Run("семантика выключена (ключ не настроен) — full-text продолжает работать", func(t *testing.T) {
		searcher := &Searcher{Embedder: nil, Items: items}
		if searcher.SemanticAvailable() {
			t.Fatal("Searcher без векторизатора считает семантику доступной")
		}
		got, err := searcher.Search(ctx, pool, scopeA, "кофе", 10)
		if err != nil {
			t.Fatalf("Search без векторизатора: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("полнотекстовых попаданий %d, хочу 2", len(got))
		}
		for _, s := range got {
			if s.Distance >= 0 {
				t.Error("семантическая дистанция при выключенном векторизаторе")
			}
		}
		if _, err := IndexPending(ctx, pool, nil, items, 10); !errors.Is(err, ErrSemanticUnavailable) {
			t.Errorf("IndexPending без векторизатора: err = %v, хочу ErrSemanticUnavailable", err)
		}
	})

	t.Run("limit режет объединённую выдачу", func(t *testing.T) {
		searcher := &Searcher{Embedder: emb, Items: items}
		got, err := searcher.Search(ctx, pool, scopeA, "кофе", 2)
		if err != nil {
			t.Fatalf("Search(limit=2): %v", err)
		}
		if len(got) != 2 {
			t.Errorf("найдено %d, хочу не больше 2", len(got))
		}
	})
}

func TestIndexPendingReindex(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	items := Items{}
	emb := mapEmbedder{vectors: map[string][]float32{"запись первая": {1, 0}, "запись вторая": {0, 1}}}

	seedItem(t, ctx, pool, scopeA, "запись первая", seedOpts{})
	seedItem(t, ctx, pool, scopeA, "запись вторая", seedOpts{})

	if n, err := IndexPending(ctx, pool, emb, items, 100); err != nil || n != 2 {
		t.Fatalf("первая индексация: n=%d err=%v, хочу 2", n, err)
	}
	if n, err := IndexPending(ctx, pool, emb, items, 100); err != nil || n != 0 {
		t.Fatalf("повторная индексация: n=%d err=%v, хочу 0 (без векторов другой модели)", n, err)
	}

	// superseded не индексируется заново
	second := seedItem(t, ctx, pool, scopeA, "запись третья", seedOpts{})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	provID, err := (SourceRepo{}).EnsureProvenance(ctx, tx, SourceJournalEvent, nextRef("prov"), []string{scopeA})
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if _, err := items.Supersede(ctx, tx, second.ID, NewItem{
		Kind: KindClaim, Text: "запись вторая, новая редакция", Salience: 0.5,
		ProvenanceID: provID, Source: &SourceRef{Kind: SourceJournalEvent, Ref: nextRef("turn")},
	}, scopeA); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Активных записей три (четвёртая — «запись третья» — заменена и потому
	// superseded); все три переиндексируются под новую модель.
	multi := multiEmbedder{fallback: []float32{1, 1, 1}}
	if n, err := IndexPending(ctx, pool, multi, items, 100); err != nil || n != 3 {
		t.Fatalf("переиндексация при смене модели: n=%d err=%v, хочу 3 активных (superseded пропущен)", n, err)
	}

	var supersededVectors int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_embeddings e
		 JOIN memory_items m ON m.id = e.memory_id
		 WHERE m.status = 'superseded'`).Scan(&supersededVectors); err != nil {
		t.Fatalf("векторы superseded-записей не читаются: %v", err)
	}
	if supersededVectors != 0 {
		t.Errorf("superseded-записи индексируются (%d векторов), хочу 0 — они не участвуют в поиске", supersededVectors)
	}
	var activeModels int
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT e.model) FROM memory_embeddings e
		 JOIN memory_items m ON m.id = e.memory_id
		 WHERE m.status = 'active'`).Scan(&activeModels); err != nil {
		t.Fatalf("модели активных не читаются: %v", err)
	}
	if activeModels != 1 {
		t.Errorf("моделей у активных записей = %d, хочу 1 (все на новой)", activeModels)
	}
}

// multiEmbedder возвращает один и тот же вектор на любой текст.
type multiEmbedder struct {
	fallback []float32
}

func (m multiEmbedder) Model() string { return "multi-embedder-v2" }

func (m multiEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = m.fallback
	}
	return out, nil
}

// TestSearchFTSHyphenQuery — 17.13: дефисный запрос ищет слова по отдельности
// с OR-семантикой: «арка-фанера» находит раздельное написание «арка из
// фанеры» (до фикса дефис оставался в токене и to_tsquery получал фразовую
// семантику — проверено на живой БД). Обычные многословные запросы не
// регрессируют: ранжирование покрытием запроса прежнее.
func TestSearchFTSHyphenQuery(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	separate := seedItem(t, ctx, pool, scopeA, "Мастер сделал арку из фанеры за вечер", seedOpts{salience: 0.7})
	hyphen := seedItem(t, ctx, pool, scopeA, "Чертёж арка-фанера утверждён без правок", seedOpts{salience: 0.5})

	found, err := (Items{}).SearchFTS(ctx, pool, scopeA, "арка-фанера", 10)
	if err != nil {
		t.Fatalf("SearchFTS(арка-фанера): %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("дефисный запрос нашёл %d записей, хочу 2 (раздельное и дефисное написание): %v",
			len(found), texts(found))
	}
	// Раздельное написание покрыто обоими словами запроса — выше рангом.
	if found[0].ID != separate.ID || found[1].ID != hyphen.ID {
		t.Fatalf("порядок = %v, хочу сначала раздельное написание %d", itemIDs(found), separate.ID)
	}

	// Обычный запрос без дефиса работает как раньше.
	plain, err := (Items{}).SearchFTS(ctx, pool, scopeA, "чертёж", 10)
	if err != nil {
		t.Fatalf("SearchFTS(чертёж): %v", err)
	}
	if len(plain) != 1 || plain[0].ID != hyphen.ID {
		t.Fatalf("обычный запрос регрессировал: %v", texts(plain))
	}
}
