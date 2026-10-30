// Индексация эмбеддингов (спека §10.4 п.4, §18): активные записи без вектора
// или с вектором другой модели получают вектор настроенного векторизатора.
// Вектор без размерности — переиндексация при смене EMBEDDING_MODEL; смена
// текста записи идёт через supersede (новая запись — новый вектор), поэтому
// досрочного переосвидетельствования по тексту не нужно.
package memory

import (
	"context"
	"fmt"
)

// embedBatch — размер пакета векторизации.
const embedBatch = 32

// IndexPending векторизует до limit активных записей, у которых нет вектора
// или вектор другой модели; возвращает число проиндексированных. Вызывается
// после записи хода (9.4) или консолидацией (9.6); частичная индексация не
// мешает поиску — векторы просто появляются постепенно.
func IndexPending(ctx context.Context, db DBTX, emb Embedder, items Items, limit int) (int, error) {
	if emb == nil {
		return 0, ErrSemanticUnavailable
	}
	if limit <= 0 {
		limit = embedBatch
	}

	rows, err := db.Query(ctx,
		`SELECT m.id, m.text FROM memory_items m
		 LEFT JOIN memory_embeddings e ON e.memory_id = m.id
		 WHERE m.status = 'active' AND (e.memory_id IS NULL OR e.model <> $1)
		 ORDER BY m.id
		 LIMIT $2`,
		emb.Model(), limit)
	if err != nil {
		return 0, fmt.Errorf("memory_embeddings: выборка без векторов: %w", err)
	}
	type pending struct {
		id   int64
		text string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.text); err != nil {
			rows.Close()
			return 0, fmt.Errorf("memory_embeddings: чтение строки: %w", err)
		}
		batch = append(batch, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("memory_embeddings: обход выборки: %w", err)
	}
	rows.Close()

	indexed := 0
	for start := 0; start < len(batch); start += embedBatch {
		end := start + embedBatch
		if end > len(batch) {
			end = len(batch)
		}
		chunk := batch[start:end]
		texts := make([]string, len(chunk))
		for i, p := range chunk {
			texts[i] = p.text
		}
		vectors, err := emb.Embed(ctx, texts)
		if err != nil {
			return indexed, fmt.Errorf("memory_embeddings: векторизация: %w", err)
		}
		if len(vectors) != len(chunk) {
			return indexed, fmt.Errorf("memory_embeddings: %d векторов на %d записей", len(vectors), len(chunk))
		}
		for i, p := range chunk {
			if _, err := db.Exec(ctx,
				`INSERT INTO memory_embeddings (memory_id, model, embedding)
				 VALUES ($1, $2, $3::vector)
				 ON CONFLICT (memory_id) DO UPDATE
				 SET model = EXCLUDED.model, embedding = EXCLUDED.embedding, updated_at = now()`,
				p.id, emb.Model(), vectorLiteral(vectors[i])); err != nil {
				return indexed, fmt.Errorf("memory_embeddings: вставка вектора записи %d: %w", p.id, err)
			}
			indexed++
		}
	}
	return indexed, nil
}
