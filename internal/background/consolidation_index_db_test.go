//go:build db

// БД-тест индексации эмбеддингов после консолидации (16.8, §10.4): summary
// эпизода получает вектор сразу после коммита консолидации — семантический
// канал поиска видит производную память фона.
package background

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/memory"
)

// TestConsolidationIndexesEmbeddings — после консолидации эпизода summary
// проиндексирован векторизатором из инструментария памяти фона.
func TestConsolidationIndexesEmbeddings(t *testing.T) {
	f, ctx := newFixture(t)

	// Память фона — с фейковым векторизатором (как при настроенном EMBEDDING_*).
	toolset, err := memory.NewToolset(f.pool, &memory.Searcher{
		Embedder: memory.NewHashEmbedder(), Items: memory.Items{}})
	if err != nil {
		t.Fatalf("toolset памяти не создан: %v", err)
	}
	f.sched.memory = toolset

	f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: consolidationJSON(
			"Эпизод про индексацию: обсудили встраивания памяти.",
			"Интересуется эмбеддингами")}},
	)
	f.seedEpisode(t, ctx, bgChatID, "private", 3, f.now.Add(-5*time.Hour))

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт консолидации не удался: %v", err)
	}

	var n int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_embeddings e
		JOIN memory_items m ON m.id = e.memory_id
		WHERE m.kind = 'experience' AND m.text LIKE 'Эпизод про индексацию%'`).Scan(&n); err != nil {
		t.Fatalf("memory_embeddings не читается: %v", err)
	}
	if n != 1 {
		t.Fatalf("summary консолидации без вектора: строк %d, хочу 1 (16.8)", n)
	}
}

// failingEmbedder — векторизатор, всегда падающий: проверка контракта лучших
// усилий (16.8) — ошибка индексации не роняет закоммиченную консолидацию.
type failingEmbedder struct{}

func (failingEmbedder) Model() string { return "failing-embedder" }

func (failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("векторизатор недоступен")
}

// TestConsolidationSurvivesEmbedderFailure — сбойный векторизатор: такт
// завершается без ошибки, эпизод закоммичен (summarized_at), векторов нет.
func TestConsolidationSurvivesEmbedderFailure(t *testing.T) {
	f, ctx := newFixture(t)
	toolset, err := memory.NewToolset(f.pool, &memory.Searcher{
		Embedder: failingEmbedder{}, Items: memory.Items{}})
	if err != nil {
		t.Fatalf("toolset памяти не создан: %v", err)
	}
	f.sched.memory = toolset

	f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: consolidationJSON(
			"Эпизод с битым векторизатором.", "Спокойно относится к сбоям")}},
	)
	f.seedEpisode(t, ctx, bgChatID, "private", 3, f.now.Add(-5*time.Hour))

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт консолидации не должен падать от ошибки индексации: %v", err)
	}

	var summarized int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM social_events
		WHERE chat_id = $1 AND summarized_at IS NOT NULL`, bgChatID).Scan(&summarized); err != nil {
		t.Fatalf("события эпизода не читаются: %v", err)
	}
	if summarized != 3 {
		t.Fatalf("эпизод не закоммичен: summarized=%d из 3", summarized)
	}
	var n int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_embeddings e
		JOIN memory_items m ON m.id = e.memory_id
		WHERE m.kind = 'experience' AND m.text LIKE 'Эпизод с битым векторизатором%'`).Scan(&n); err != nil {
		t.Fatalf("memory_embeddings не читается: %v", err)
	}
	if n != 0 {
		t.Fatalf("векторизатор падает — векторов быть не должно, есть %d", n)
	}
}
