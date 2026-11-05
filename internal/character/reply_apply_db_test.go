//go:build db

// БД-тесты реплаев на старые сообщения (этап 37.2): метка из этого чата
// превращается в reply_parameters, чужая/неизвестная — сообщение уходит без
// реплая; метка стабильна между ходами, потому что это Telegram message_id.
package character

import (
	"testing"
)

func TestApplyTurnReplyToOldMessageDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3721)
	f.seedTurn(t, ctx, turnID, "ответь на старое")

	// Старое сообщение собеседника в этом же чате.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, telegram_message_id)
		VALUES (3721001, $1, 'private', 777, 'Коля', 'message', 'старое сообщение', '{}'::jsonb, 'processed', 8801)`,
		testChatID); err != nil {
		t.Fatalf("старое сообщение не вставлено: %v", err)
	}
	// Доставленный ответ бота тоже имеет метку.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, telegram_message_id)
		VALUES (3721002, 1, $1, '{"kind":"message","text":"старый ответ бота"}'::jsonb, 'sent', 8802)`,
		testChatID); err != nil {
		t.Fatalf("старый ответ бота не вставлен: %v", err)
	}

	res, err := ApplyTurn(ctx, f.pool, ApplyOptions{
		TurnID:  turnID,
		ChatID:  testChatID,
		ScopeID: testScopeID,
		Result: TurnResult{Decision: DecisionAnswer, Messages: []string{"вот ответ"},
			ReplyToByMessage: []MessageReply{{Index: 1, Ref: "#m8801"}}},
		PersonaBaseline: f.persona.MoodBaseline,
		Journal:         f.journal,
	})
	if err != nil {
		t.Fatalf("применение: %v", err)
	}
	if !res.Applied || len(res.SkippedReplies) != 0 {
		t.Fatalf("применение = %+v, skipped=%+v", res, res.SkippedReplies)
	}
	var replyTo int64
	if err := f.pool.QueryRow(ctx, `
		SELECT COALESCE((payload->>'reply_to_message_id')::bigint, 0)
		FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'message'`, turnID).Scan(&replyTo); err != nil {
		t.Fatalf("outbox не прочитан: %v", err)
	}
	if replyTo != 8801 {
		t.Fatalf("reply_to_message_id = %d, хочу 8801", replyTo)
	}

	// Реплай на отправленный ответ бота этим же механизмом.
	f2, ctx2 := newFixture(t)
	turnID2 := int64(3722)
	f2.seedTurn(t, ctx2, turnID2, "ответь на мой старый ответ")
	if _, err := f2.pool.Exec(ctx2, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, telegram_message_id)
		VALUES (3722001, 1, $1, '{"kind":"message","text":"старый ответ"}'::jsonb, 'sent', 8811)`,
		testChatID); err != nil {
		t.Fatalf("ответ бота не вставлен: %v", err)
	}
	if _, err := ApplyTurn(ctx2, f2.pool, ApplyOptions{
		TurnID:  turnID2,
		ChatID:  testChatID,
		ScopeID: testScopeID,
		Result: TurnResult{Decision: DecisionAnswer, Messages: []string{"реплай на себя"},
			ReplyToByMessage: []MessageReply{{Index: 1, Ref: "#m8811"}}},
		PersonaBaseline: f2.persona.MoodBaseline,
		Journal:         f2.journal,
	}); err != nil {
		t.Fatalf("применение реплая на ответ бота: %v", err)
	}
	if err := f2.pool.QueryRow(ctx2, `
		SELECT COALESCE((payload->>'reply_to_message_id')::bigint, 0)
		FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'message'`, turnID2).Scan(&replyTo); err != nil {
		t.Fatalf("outbox реплая на ответ бота: %v", err)
	}
	if replyTo != 8811 {
		t.Fatalf("reply_to_message_id = %d, хочу 8811", replyTo)
	}
}

func TestApplyTurnReplyUnknownOrForeignKeyStaysPlainDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3723)
	f.seedTurn(t, ctx, turnID, "ответь")

	// Сообщение с таким Telegram id есть, но в ДРУГОМ чате (§9.1: между
	// чатами нельзя).
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, telegram_message_id)
		VALUES (3723001, -777001, 'private', 777, 'Коля', 'message', 'чужой чат', '{}'::jsonb, 'processed', 9901)`,
	); err != nil {
		t.Fatalf("чужое сообщение не вставлено: %v", err)
	}

	res, err := ApplyTurn(ctx, f.pool, ApplyOptions{
		TurnID:  turnID,
		ChatID:  testChatID,
		ScopeID: testScopeID,
		Result: TurnResult{Decision: DecisionAnswer, Messages: []string{"без реплая"},
			ReplyToByMessage: []MessageReply{
				{Index: 1, Ref: "#m9901"}, // чужой чат
			}},
		PersonaBaseline: f.persona.MoodBaseline,
		Journal:         f.journal,
	})
	if err != nil {
		t.Fatalf("применение: %v", err)
	}
	if !res.Applied {
		t.Fatal("ход не применён")
	}
	if len(res.SkippedReplies) != 1 {
		t.Fatalf("чужая метка не отмечена: %+v", res.SkippedReplies)
	}
	var replyTo *string
	if err := f.pool.QueryRow(ctx, `
		SELECT payload->>'reply_to_message_id' FROM delivery_outbox
		WHERE turn_id = $1 AND payload->>'kind' = 'message'`, turnID).Scan(&replyTo); err != nil {
		t.Fatalf("reply_to_message_id не прочитан: %v", err)
	}
	if replyTo != nil {
		t.Fatalf("чужая метка дала реплай: %s", *replyTo)
	}
}
