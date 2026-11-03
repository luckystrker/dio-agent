//go:build db

// Этап 31.2: план инструментов памяти разговорного хода — durable событие
// журнала, а не строка лога. Метрики считают причины включения инструментов
// (history_trimmed/card_incomplete/...) по журналу (философия №2), поэтому
// событие обязано писаться один раз и быть читаемым при восстановлении.
package character

import (
	"context"
	"testing"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

func TestTurnPlanEventDB(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	journal := NewTurnJournal(runtime.NewPgStreamStorage(ctx, pool), nil)
	const turnID int64 = 42

	plan := &TurnPlan{
		TurnID:      turnID,
		Enabled:     true,
		Reason:      "card_incomplete",
		CardEntries: 3,
		CardFitted:  false,
	}
	if err := journal.WriteTurnPlan(ctx, turnID, plan); err != nil {
		t.Fatalf("turn_plan не записан: %v", err)
	}
	// Повтор (retry хода с тем же планом) не плодит событий: appendOnce.
	if err := journal.WriteTurnPlan(ctx, turnID, plan); err != nil {
		t.Fatalf("повторный turn_plan не записан: %v", err)
	}

	evs, err := journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал не прочитан: %v", err)
	}
	found := 0
	for _, ev := range evs {
		got, ok := events.AsValue(ev).(TurnPlan)
		if !ok {
			continue
		}
		found++
		if !got.Enabled || got.Reason != "card_incomplete" || got.CardEntries != 3 || got.CardFitted {
			t.Fatalf("turn_plan прочитан неверно: %+v", got)
		}
	}
	if found != 1 {
		t.Fatalf("turn_plan событий = %d, хочу ровно 1 (однократная запись)", found)
	}
}

// TestTurnPlanEventFromRunnerDB — проводка: разговорный ход реально пишет
// turn_plan до модельного вызова (метрики §20 не парсят логи).
func TestTurnPlanEventFromRunnerDB(t *testing.T) {
	f, ctx := newFixture(t)
	const turnID int64 = 501
	f.seedTurn(t, ctx, turnID, "Привет, как дела?")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: validFinalJSON()}})

	if _, err := f.runner.Run(ctx, Input{
		TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Привет, как дела?"}},
	}); err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}

	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал не прочитан: %v", err)
	}
	var plan *TurnPlan
	for _, ev := range evs {
		if got, ok := events.AsValue(ev).(TurnPlan); ok {
			plan = &got
			break
		}
	}
	if plan == nil {
		t.Fatalf("turn_plan не записан ходом; события: %v", evs)
	}
	// Фикстура собирает Runner без предзагрузки памяти (MODEL_MEMORY_PREFETCH
	// выключен) — причина «rollback», инструменты выдаются как раньше.
	if plan.Reason != "rollback" {
		t.Errorf("причина turn_plan = %q, хочу rollback", plan.Reason)
	}
	if plan.TurnID != turnID {
		t.Errorf("turn_plan.turn_id = %d, хочу %d", plan.TurnID, turnID)
	}
}
