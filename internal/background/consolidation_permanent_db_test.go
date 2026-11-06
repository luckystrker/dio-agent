//go:build db

// БД-тест этапа 38.6: постоянный отказ провайдера (400 invalid_request_error
// «this feature is not yet supported by atlas», живой прогон 2026-11-06) не
// жжёт пять попыток одинаковым запросом — консолидация терминально падает с
// cooldown 24 ч, как невалидный proposal, и соседние эпизоды не ждут.
package background

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/character"
)

// failingModel — модельный конфиг, который отказывает с заданной ошибкой.
type failingModel struct {
	err   error
	calls atomic.Int32
}

func (m *failingModel) Provider() config.Provider { return config.ProviderOpenAI }
func (m *failingModel) Model() string             { return "test" }
func (m *failingModel) Create() (config.LLMClient, error) {
	return m, nil
}
func (m *failingModel) Clone(map[string]any) (config.ModelConfig, error) { return m, nil }
func (m *failingModel) Call(context.Context, []events.Event, *agocontext.Context, config.CallOptions) (*events.ModelResponse, error) {
	m.calls.Add(1)
	return nil, m.err
}

func TestConsolidationPermanent400StopsAfterFirstAttempt(t *testing.T) {
	f, ctx := newFixture(t)
	model := &failingModel{err: errors.New(`config: chat completions returned 400: {"error":{"type":"invalid_request_error","message":"Upstream request failed: [invalid_request_error] this feature is not yet supported by atlas"}}`)}
	f.sched.model = character.NewChatConfigWithClient(model)

	ids, _ := f.seedEpisode(t, ctx, -548919987, "private", 3, f.now.Add(-5*time.Hour))
	ep := Episode{ChatID: -548919987, ChatType: "private", EventIDs: ids, LastEventID: ids[len(ids)-1]}

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}
	if got := model.calls.Load(); got != 1 {
		t.Fatalf("модельных вызовов %d, хочу 1 — 400 не транзиентен, повторять его нечем", got)
	}

	cooled, cerr := f.sched.consolidationCooled(ctx, ep)
	if cerr != nil {
		t.Fatalf("cooldown не читается: %v", cerr)
	}
	if !cooled {
		t.Fatal("постоянный 400 обязан поставить cooldown 24 ч, иначе каждый такт повторяет тот же запрос")
	}

	var unsummarized int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NULL`, ids).Scan(&unsummarized); err != nil {
		t.Fatalf("события не читаются: %v", err)
	}
	if unsummarized != len(ids) {
		t.Fatalf("события эпизода помечены сведёнными (%d из %d свободны) — суммаризации не было", unsummarized, len(ids))
	}

	// Следующий такт: эпизод остывает — повторного запроса нет.
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("второй такт не удался: %v", err)
	}
	if got := model.calls.Load(); got != 1 {
		t.Fatalf("после cooldown-паузы модельных вызовов %d, хочу 1", got)
	}
}
