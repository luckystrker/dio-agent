//go:build db

// БД-тесты пересылки (этап 37.3): forward → outbox kind=forward после
// сообщений, только message_id этого чата (§9.1), неизвестная метка — мягко
// обнуляется с логом.
package character

import "testing"

func TestApplyTurnForwardsWithinChatDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3731)
	f.seedTurn(t, ctx, turnID, "напомни старое")

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, telegram_message_id)
		VALUES (3731001, $1, 'private', 777, 'Коля', 'message', 'старое сообщение', '{}'::jsonb, 'processed', 9301)`,
		testChatID); err != nil {
		t.Fatalf("старое сообщение не вставлено: %v", err)
	}

	res, err := ApplyTurn(ctx, f.pool, ApplyOptions{
		TurnID:  turnID,
		ChatID:  testChatID,
		ScopeID: testScopeID,
		Result: TurnResult{Decision: DecisionAnswer, Messages: []string{"напоминаю"},
			Forward: "#m9301"},
		PersonaBaseline: f.persona.MoodBaseline,
		Journal:         f.journal,
	})
	if err != nil {
		t.Fatalf("применение: %v", err)
	}
	if !res.Applied || res.SkippedForward != nil || res.ForwardOutboxID == 0 {
		t.Fatalf("применение = %+v", res)
	}

	var (
		position int
		message  int64
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT position, (payload->>'forward_message_id')::bigint
		FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'forward'`, turnID).
		Scan(&position, &message); err != nil {
		t.Fatalf("пузырь пересылки не создан: %v", err)
	}
	if position != 1 {
		t.Fatalf("позиция пересылки %d, хочу 1 (после сообщения хода)", position)
	}
	if message != 9301 {
		t.Fatalf("forward_message_id = %d", message)
	}
}

func TestApplyTurnForwardForeignKeySkippedDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3732)
	f.seedTurn(t, ctx, turnID, "перешли чужое")

	// Тот же Telegram message_id существует, но в другом чате.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, telegram_message_id)
		VALUES (3732001, -777002, 'private', 777, 'Коля', 'message', 'чужое', '{}'::jsonb, 'processed', 9401)`,
	); err != nil {
		t.Fatalf("чужое сообщение не вставлено: %v", err)
	}

	res, err := ApplyTurn(ctx, f.pool, ApplyOptions{
		TurnID:  turnID,
		ChatID:  testChatID,
		ScopeID: testScopeID,
		Result: TurnResult{Decision: DecisionAnswer, Messages: []string{"ок"},
			Forward: "#m9401"},
		PersonaBaseline: f.persona.MoodBaseline,
		Journal:         f.journal,
	})
	if err != nil {
		t.Fatalf("применение: %v", err)
	}
	if !res.Applied {
		t.Fatal("ход не применён")
	}
	if res.SkippedForward == nil || res.ForwardOutboxID != 0 {
		t.Fatalf("чужой чат дал пересылку: %+v", res)
	}
	var forwards int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'forward'`, turnID).
		Scan(&forwards); err != nil {
		t.Fatalf("outbox не прочитан: %v", err)
	}
	if forwards != 0 {
		t.Fatalf("пузырей пересылки %d, хочу 0", forwards)
	}
}
