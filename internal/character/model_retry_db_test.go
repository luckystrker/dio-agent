//go:build db

// БД-тест этапа 38.5: короткий повтор 429 внутри вызова не резервирует бюджет
// второй раз. Middleware §16 резервирует один operation_id до модельного
// вызова и не знает о внутренних повторах — это и проверяем на боевой схеме.
package character

import (
	"context"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

func TestShort429RetryReservesBudgetOnce(t *testing.T) {
	f, _ := newFixture(t)
	stubRetrySleep(t)
	server, calls := retryServer(t, 1, short429Body)

	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	cfg.Logf = func(string, ...any) {}
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}

	const op = "model:turn:38:1"
	mw := NewBudgetMiddleware(f.budget, op, 10, discardLogger())
	resp, err := mw.OnLLMCall(func(evs []events.Event, cx *agocontext.Context) (*events.ModelResponse, error) {
		return client.Call(context.Background(), evs, cx, config.CallOptions{})
	}, nil, nil)
	if err != nil || resp == nil {
		t.Fatalf("вызов с коротким повтором не удался: resp=%v err=%v", resp, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("провайдер получил %d запросов, хочу 2 (один короткий повтор)", got)
	}

	rows, hasActual := budgetReservationState(t, f, op+":0")
	if rows != 1 || !hasActual {
		t.Fatalf("резервов %d, actual записан=%v — хочу один резерв на логический вызов (§16)", rows, hasActual)
	}
	var reserved int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT reserved_calls FROM budget_counters WHERE pool = $1 AND period = CURRENT_DATE`,
		runtime.BudgetPoolConversation).Scan(&reserved); err != nil {
		t.Fatalf("счётчик дня не читается: %v", err)
	}
	if reserved != 1 {
		t.Fatalf("дневной счётчик %d, хочу 1 — повтор 429 не должен жечь бюджет дважды", reserved)
	}
}
