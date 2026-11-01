//go:build db

// БД-тесты индексации эмбеддингов после хода (16.8, §10.4): применение хода,
// записывающего память, наполняет memory_embeddings векторизатором из
// инструментария памяти; повторные прогоны не дублируют строки. Семантический
// канал поиска оживает в прод-пути, а не только в прямых вызовах тестов.
package character

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/memory"
)

// embeddingsForRef — строки memory_embeddings у записей с данным источником.
func embeddingsForRef(t *testing.T, ctx context.Context, f *fixture, ref string) int {
	t.Helper()
	var n int
	if err := f.runner.Pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_embeddings e
		JOIN memory_sources ms ON ms.memory_id = e.memory_id
		JOIN sources s ON s.id = ms.source_id AND s.ref = $1`, ref).Scan(&n); err != nil {
		t.Fatalf("memory_embeddings не читается: %v", err)
	}
	return n
}

// withEmbedderMemory — инструментарий памяти с фейковым векторизатором: как в
// сборке приложения при настроенном EMBEDDING_* (embedder живёт в Toolset,
// второй инстанс не создаётся).
func (f *fixture) withEmbedderMemory(t *testing.T, ctx context.Context) {
	t.Helper()
	toolset, err := memory.NewToolset(f.runner.Pool, &memory.Searcher{
		Embedder: memory.NewHashEmbedder(), Items: memory.Items{}})
	if err != nil {
		t.Fatalf("toolset памяти не создан: %v", err)
	}
	f.runner.Memory = toolset
}

// TestTurnIndexesMemoryEmbeddings — ход, записывающий память, векторизует её
// после применения (16.8): строки появляются в memory_embeddings.
func TestTurnIndexesMemoryEmbeddings(t *testing.T) {
	f, ctx := newFixture(t)
	f.withEmbedderMemory(t, ctx)

	turnID := int64(120)
	f.seedTurn(t, ctx, turnID, "запиши: пластинки это важно")
	f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"ignore",` +
			`"memory":[{"kind":"preference","text":"Пластинки для Коли святы","salience":0.6}],` +
			`"reason":"запомнил"}`}},
	)
	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "запиши: пластинки это важно"}}})
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	if !out.Apply.Applied || len(out.Apply.MemoryIDs) != 1 {
		t.Fatalf("ход не применил кандидата памяти: %+v", out)
	}

	ref := fmt.Sprintf("turn:%d", turnID)
	if n := embeddingsForRef(t, ctx, f, ref); n != 1 {
		t.Fatalf("у кандидата памяти %d векторов, хочу 1: семантический канал §10.4 мёртв в прод-пути (16.8)", n)
	}

	// Повторный прогон (ход уже применён) не дублирует векторы.
	out2, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID})
	if err != nil {
		t.Fatalf("повторный прогон упал: %v", err)
	}
	if !out2.AlreadyApplied {
		t.Fatalf("повторный прогон должен быть no-op: %+v", out2)
	}
	if n := embeddingsForRef(t, ctx, f, ref); n != 1 {
		t.Fatalf("после повторного прогона векторов %d, хочу 1 (без дублей)", n)
	}
}

// failingEmbedder — векторизатор, всегда падающий: проверка контракта лучших
// усилий (16.8) — ошибка индексации не трогает применённый ход.
type failingEmbedder struct{}

func (failingEmbedder) Model() string { return "failing-embedder" }

func (failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("векторизатор недоступен")
}

// TestTurnApplySurvivesEmbedderFailure — сбой векторизатора после применения
// хода — только запись в лог: Run не возвращает ошибку, ход применён, записей
// памяти с вектором нет (догонят позже, когда векторизатор оживёт).
func TestTurnApplySurvivesEmbedderFailure(t *testing.T) {
	f, ctx := newFixture(t)
	toolset, err := memory.NewToolset(f.runner.Pool, &memory.Searcher{
		Embedder: failingEmbedder{}, Items: memory.Items{}})
	if err != nil {
		t.Fatalf("toolset памяти не создан: %v", err)
	}
	f.runner.Memory = toolset

	turnID := int64(121)
	f.seedTurn(t, ctx, turnID, "запиши: чай важнее кофе")
	f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"ignore",` +
			`"memory":[{"kind":"preference","text":"Чай важнее кофе","salience":0.5}],` +
			`"reason":"запомнил"}`}},
	)
	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "запиши: чай важнее кофе"}}})
	if err != nil {
		t.Fatalf("ход не должен падать от ошибки индексации: %v", err)
	}
	if !out.Apply.Applied || len(out.Apply.MemoryIDs) != 1 {
		t.Fatalf("ход должен быть применён несмотря на сбой векторизатора: %+v", out)
	}
	if n := embeddingsForRef(t, ctx, f, fmt.Sprintf("turn:%d", turnID)); n != 0 {
		t.Fatalf("векторизатор падает — векторов быть не должно, есть %d", n)
	}
}
