//go:build db

// БД-тесты этапа 38.2: answer без текста после repair не теряет ход —
// деградация в react (реакция/стикер остаются) или ignore.
package character

import (
	"testing"

	"github.com/luckystrker/ago/events"
)

// TestRunDegradesEmptyAnswerToReactDB — модель дважды вернула answer без
// сообщений, но с реакцией: ход применяется как react, реакция уезжает в
// outbox. Терминального пропуска нет.
func TestRunDegradesEmptyAnswerToReactDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.seedTurn(t, ctx, 2101, "ну?")
	f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","reaction":"🤔"}`}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","reaction":"🤔"}`}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: 2101, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "ну?"}}})
	if err != nil {
		t.Fatalf("ход: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("пустой answer деградировал не в react, а в пропуск: %+v", out)
	}
	if out.LLMCalls != 2 {
		t.Fatalf("модельных вызовов %d, хочу 2 (первый + repair)", out.LLMCalls)
	}
	rows := f.outboxRows(t, ctx, 2101)
	if len(rows) != 1 || rows[0]["kind"] != "reaction" || rows[0]["reaction"] != "🤔" {
		t.Fatalf("ожидалась одна реакция в outbox: %v", rows)
	}
}

// TestRunDegradesEmptyAnswerToIgnoreDB — после repair ни реакции, ни стикера:
// тихий ignore, без сообщений и без терминального пропуска.
func TestRunDegradesEmptyAnswerToIgnoreDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.seedTurn(t, ctx, 2102, "ну?")
	f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer"}`}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer"}`}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: 2102, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "ну?"}}})
	if err != nil {
		t.Fatalf("ход: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("пустой answer без доставки не стал ignore: %+v", out)
	}
	if rows := f.outboxRows(t, ctx, 2102); len(rows) != 0 {
		t.Fatalf("ignore что-то отправил: %v", rows)
	}
}
