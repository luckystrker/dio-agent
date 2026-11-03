// Полнотекстовый поиск с покрытием запроса (28.3): OR-семантика §10.4 ищет
// записи по ЛЮБОМУ слову запроса, поэтому «попадание» ещё не значит
// «релевантно»: в живом чате с частыми словами (ремонт/шпаклёвка/кот) длинная
// пачка находила 9 записей, и девятая делала карточку неполной. Здесь считается
// покрытие: ts_rank записи относительно лучшего достижимого ts_rank для этого
// запроса (вектор из самого запроса). Запись, совпавшая с одним словом из
// двадцати, честно получает ~0.05 и не считается вытесненным релевантным;
// запись, покрывшая большинство слов, — близко к 1. Для запроса из одного слова
// покрытие совпадения — 1: единственное слово и есть весь запрос.
package memory

import (
	"context"
	"fmt"
)

// SearchFTSRanked — полнотекстовый поиск с Rank (покрытие запроса 0..1).
// Остальное поведение то же, что у SearchFTS: русская морфология, слова через
// ИЛИ, фильтр видимости до ранжирования, порядок «ранг, значимость, время».
func (r Items) SearchFTSRanked(ctx context.Context, db DBTX, audience, query string, limit int) ([]ScoredItem, error) {
	if !IsValidScope(audience) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	q := orTsQuery(query)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	// Лучший достижимый ранг — вектор из слов самого запроса: эталон полного
	// покрытия при той же русской конфигурации и той же tsquery.
	var ideal float64
	if err := db.QueryRow(ctx,
		`SELECT ts_rank(to_tsvector('russian', $1), to_tsquery('russian', $2))`,
		query, q).Scan(&ideal); err != nil {
		return nil, fmt.Errorf("memory_items: эталон ранга запроса: %w", err)
	}

	rows, err := db.Query(ctx,
		`SELECT `+prefixedItemCols("m")+`,
		        ts_rank(to_tsvector('russian', m.text), to_tsquery('russian', $2)) AS rank
		 FROM memory_items m
		 LEFT JOIN provenance p ON p.id = m.provenance_id
		 WHERE m.status = 'active'
		   AND `+visCond("m", 1)+`
		   AND to_tsvector('russian', m.text) @@ to_tsquery('russian', $2)
		 ORDER BY rank DESC, m.salience DESC, m.recorded_at DESC
		 LIMIT $3`,
		audience, q, limit)
	if err != nil {
		return nil, fmt.Errorf("memory_items: полнотекстовый поиск с рангом: %w", err)
	}
	defer rows.Close()

	var out []ScoredItem
	for rows.Next() {
		var s ScoredItem
		if err := rows.Scan(&s.Item.ID, &s.Item.Kind, &s.Item.Text, &s.Item.ScopeID, &s.Item.Status,
			&s.Item.Salience, &s.Item.Confidence, &s.Item.OccurredAt, &s.Item.RecordedAt,
			&s.Item.ValidFrom, &s.Item.ValidUntil, &s.Item.SupersedesID, &s.Item.ProvenanceID,
			&s.Rank); err != nil {
			return nil, fmt.Errorf("memory_items: чтение строки полнотекста: %w", err)
		}
		s.Distance = -1
		if ideal > 0 {
			s.Rank /= ideal
			if s.Rank > 1 {
				s.Rank = 1
			}
		} else {
			s.Rank = 0
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
