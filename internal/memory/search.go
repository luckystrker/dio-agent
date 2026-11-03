// Поиск по памяти (спека §10.4): full-text и семантический (pgvector) каналы,
// объединение и ранжирование. Фильтр приватности применяется в SQL до любого
// ранжирования — модель не получает ни текста, ни факта существования
// недоступных записей (§9.2).
package memory

import (
	"context"
	"fmt"
	"strings"
)

// ScoredItem — запись с дистанцией семантического канала; -1 — найдена
// только полнотекстом.
type ScoredItem struct {
	Item
	Distance float64
	// Rank — покрытие полнотекстового запроса 0..1 (28.3): ts_rank записи
	// относительно лучшего достижимого ранга для этого запроса. 0 у
	// семантических попаданий. Порядок выдачи уже отражает ранг; Rank нужен
	// карточке, чтобы отличить совпадение случайного слова от действительно
	// релевантной записи.
	Rank float64
}

// Searcher — объединённый поиск. Embedder == nil — семантическая ветка
// недоступна (ключ EMBEDDING_* не настроен), поиск честно работает
// полнотекстом.
type Searcher struct {
	Embedder Embedder
	Items    Items
}

// SearchRunning возвращает признак настроенной семантики.
func (s *Searcher) SemanticAvailable() bool { return s != nil && s.Embedder != nil }

// Search находит записи по запросу: full-text всегда, семантика при
// настроенном векторизаторе. Каналы сходятся в общий список: полнотекстовые
// попадания ранжируются salience/recency, семантические — дистанцией;
// объединение без дублей (§10.4 п.5: релевантность, время, значимость),
// затем фактор «разнообразие оснований», срез — не больше limit.
func (s *Searcher) Search(ctx context.Context, db DBTX, audience, query string, limit int) ([]ScoredItem, error) {
	if !IsValidScope(audience) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("%w: пустой поисковый запрос", ErrInvalidCandidate)
	}
	if limit <= 0 {
		limit = 10
	}
	// Каналы отдают кандидатов шире лимита: иначе срез канала происходит до
	// разнообразия оснований и серия записей одного источника забирает
	// выдачу, а переставлять уже нечего.
	channelPool := 3 * limit

	fts, err := s.Items.SearchFTSRanked(ctx, db, audience, q, channelPool)
	if err != nil {
		return nil, err
	}

	var semantic []ScoredItem
	if s.SemanticAvailable() {
		vectors, err := s.Embedder.Embed(ctx, []string{q})
		if err != nil {
			return nil, fmt.Errorf("memory: векторизация запроса: %w", err)
		}
		semantic, err = s.Items.SearchSemantic(ctx, db, audience, vectors[0], s.Embedder.Model(), channelPool)
		if err != nil {
			return nil, err
		}
	}

	out := make([]ScoredItem, 0, len(fts)+len(semantic))
	seen := make(map[int64]bool, len(fts)+len(semantic))
	for _, it := range fts {
		if seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		out = append(out, it)
	}
	for _, sc := range semantic {
		if seen[sc.ID] {
			continue
		}
		seen[sc.ID] = true
		out = append(out, sc)
	}

	// §10.4 п.5: разнообразие оснований — до среза, чтобы предел не забирала
	// серия записей одного источника.
	cites, err := s.Items.SourcesFor(ctx, db, itemIDs(scoredAsItems(out)))
	if err != nil {
		return nil, err
	}
	reordered := make([]ScoredItem, 0, len(out))
	for _, i := range diverseOrder(scoredAsItems(out), cites) {
		reordered = append(reordered, out[i])
	}
	if len(reordered) > limit {
		reordered = reordered[:limit]
	}
	return reordered, nil
}

// diverseOrder — стабильный жадный проход «разнообразия оснований»
// (§10.4 п.5): сначала записи, чей первичный источник ещё не встречался
// в выдаче, затем остальные в исходном порядке ранжирования. Первый элемент
// всегда остаётся первым; порядок внутри групп сохраняется; записи без
// источников образуют одну общую группу («оснований нет»).
func diverseOrder(items []Item, sources map[int64][]SourceCite) []int {
	seen := make(map[string]bool, len(items))
	first := make([]int, 0, len(items))
	rest := make([]int, 0, len(items))
	for i, it := range items {
		key := ""
		if cites := sources[it.ID]; len(cites) > 0 {
			key = cites[0].Kind + ":" + cites[0].Ref
		}
		if !seen[key] {
			seen[key] = true
			first = append(first, i)
		} else {
			rest = append(rest, i)
		}
	}
	return append(first, rest...)
}

func scoredAsItems(scored []ScoredItem) []Item {
	out := make([]Item, len(scored))
	for i, s := range scored {
		out[i] = s.Item
	}
	return out
}

func itemIDs(items []Item) []int64 {
	out := make([]int64, 0, len(items))
	seen := make(map[int64]bool, len(items))
	for _, it := range items {
		if !seen[it.ID] {
			seen[it.ID] = true
			out = append(out, it.ID)
		}
	}
	return out
}
