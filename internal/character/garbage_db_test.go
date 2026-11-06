//go:build db

// БД-тесты страховки от брака в messages (40.4): валидный JSON, но реплики —
// мусор (живые ходы 246–248). Один repair, затем выкидывание брака; все брак —
// деградация как 38.2, а не терминальный пропуск.
package character

import (
	"encoding/json"
	"testing"

	"github.com/luckystrker/ago/events"
)

func TestGarbageRepairInTurnDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3001)
	f.seedTurn(t, ctx, turnID, "Ну что там?")
	cfg := f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{
			Content: `{"decision":"answer","messages":[",","reply_to_by_message:,","привет, как ты?"]}`,
		}},
		&events.ModelResponse{Message: &events.ModelMessage{
			Content: `{"decision":"answer","messages":["привет, как ты?"]}`,
		}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Ну что там?"}}})
	if err != nil {
		t.Fatalf("ход: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("страховка не спасла ход: %+v", out)
	}
	if calls := len(cfg.Calls()); calls != 2 {
		t.Fatalf("модельных вызовов %d, хочу 2 (брак + repair)", calls)
	}

	var text string
	if err := f.pool.QueryRow(ctx,
		`SELECT payload->>'text' FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'message'`, turnID).Scan(&text); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if text != "привет, как ты?" {
		t.Fatalf("доставлен не repair-текст: %q", text)
	}
	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'message'`, turnID).Scan(&count); err != nil {
		t.Fatalf("счётчик outbox: %v", err)
	}
	if count != 1 {
		t.Fatalf("сообщений в outbox %d, хочу 1 (брак выкинут)", count)
	}
}

func TestGarbageAfterRepairAllBadDegradesDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3002)
	f.seedTurn(t, ctx, turnID, "Ответь же")
	f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{
			Content: `{"decision":"answer","messages":[","]}`,
		}},
		// Repair тоже не дал реплик: после чистки деградация в ignore.
		&events.ModelResponse{Message: &events.ModelMessage{
			Content: `{"decision":"answer","messages":[">»"]}`,
		}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Ответь же"}}})
	if err != nil {
		t.Fatalf("ход: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("все брак должны деградировать, а не пропускать ход: %+v", out)
	}
	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'message'`, turnID).Scan(&count); err != nil {
		t.Fatalf("счётчик outbox: %v", err)
	}
	if count != 0 {
		t.Fatalf("брак доставлен: %d сообщений", count)
	}
	mo := f.modelOutputOf(t, ctx, turnID)
	if mo == nil {
		t.Fatal("model_output не записан")
	}
	var res TurnResult
	if err := json.Unmarshal(mo.Result, &res); err != nil {
		t.Fatalf("model_output.result не JSON: %v", err)
	}
	if res.Decision != DecisionIgnore || len(res.Messages) != 0 {
		t.Fatalf("деградация не в ignore: %+v", res)
	}
}
