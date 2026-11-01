//go:build db

// БД-тесты возврата резерва бюджета ходом (§16, блок 17.3): отмена хода
// (context.Canceled — модельный вызов не состоялся) возвращает резерв в счётчик
// дня; прочие ошибки — стоимость неизвестна, резерв остаётся с actual unknown.
package character

import (
	"context"
	"errors"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// errNext — подмена модельного вызова: ход ответа не даёт.
type errNext struct{ err error }

func (e errNext) call([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
	return nil, e.err
}

// budgetReservationState — число строк резерва и записан ли actual (§16).
func budgetReservationState(t *testing.T, f *fixture, operationID string) (int, bool) {
	t.Helper()
	var rows int
	var hasActual bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*), count(actual) > 0 FROM budget_reservations WHERE operation_id = $1`,
		operationID).Scan(&rows, &hasActual); err != nil {
		t.Fatalf("резерв %s не читается: %v", operationID, err)
	}
	return rows, hasActual
}

// TestBudgetMiddlewareReleasesOnTurnCancel — ход отменён до завершения вызова
// (context.Canceled): резерв обязан вернуться в счётчик, не оставаясь храповиком.
func TestBudgetMiddlewareReleasesOnTurnCancel(t *testing.T) {
	f, ctx := newFixture(t)
	mw := NewBudgetMiddleware(f.budget, "model:turn:1731:1:0", 5, discardLogger())

	_, err := mw.OnLLMCall(errNext{err: context.Canceled}.call, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ошибка отмены обязана проходить наверх: %v", err)
	}

	if rows, _ := budgetReservationState(t, f, "model:turn:1731:1:0:0"); rows != 0 {
		t.Fatalf("резерв отменённого вызова не возвращён: строк %d (вызов не состоялся — день не выжигается, §16)", rows)
	}
	var reserved int64
	if err := f.pool.QueryRow(ctx,
		`SELECT reserved_calls FROM budget_counters WHERE pool = $1 AND period = CURRENT_DATE`,
		runtime.BudgetPoolConversation).Scan(&reserved); err != nil {
		t.Fatalf("счётчик дня не читается: %v", err)
	}
	if reserved != 0 {
		t.Fatalf("счётчик дня %d, хочу 0 — отмена хода не вернула резерв", reserved)
	}
}

// TestBudgetMiddlewareKeepsUnknownOnProviderError — сбой провайдера после
// запроса: стоимость неизвестна, actual не пишется нулевым (§16), резерв остаётся.
func TestBudgetMiddlewareKeepsUnknownOnProviderError(t *testing.T) {
	f, _ := newFixture(t)
	mw := NewBudgetMiddleware(f.budget, "model:turn:1732:1:0", 5, discardLogger())

	_, err := mw.OnLLMCall(errNext{err: errors.New("провайдер: HTTP 502")}.call, nil, nil)
	if err == nil {
		t.Fatal("ошибка провайдера обязана проходить наверх")
	}

	rows, hasActual := budgetReservationState(t, f, "model:turn:1732:1:0:0")
	if rows != 1 || !hasActual {
		t.Fatalf("неизвестная стоимость: резерв %d строк, actual записан=%v — хочу 1 строку с actual unknown (§16)", rows, hasActual)
	}
}
