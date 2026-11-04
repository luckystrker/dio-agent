//go:build db

// Реакция пользователя видна в недавней истории чата как событие (34.1):
// даже journal_only-реакция остаётся частью разговора для следующих ходов.
package character

import (
	"context"
	"testing"
	"time"
)

func TestRecentHistoryIncludesReactionEvents(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const chatID = int64(4242)

	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name,
			kind, text, raw_update, status, occurred_at, telegram_message_id, direct)
		VALUES (1, $1, 'private', 42, 'Коля', 'reaction',
			'[поставил 😂 на твоё «старый ответ»]', '{}'::jsonb, 'journal_only', $2, 77, false)`,
		chatID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("событие реакции не вставлено: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name,
			kind, text, raw_update, status, occurred_at, telegram_message_id, direct)
		VALUES (2, $1, 'private', 42, 'Коля', 'message', 'ну как тебе?',
			'{}'::jsonb, 'processed', $2, 78, true)`,
		chatID, time.Now()); err != nil {
		t.Fatalf("сообщение не вставлено: %v", err)
	}

	hist, err := RecentHistory(ctx, pool, chatID, 10, 0)
	if err != nil {
		t.Fatalf("история не прочитана: %v", err)
	}
	found := false
	for _, m := range hist {
		if m.Content == "[поставил 😂 на твоё «старый ответ»]" {
			found = true
		}
	}
	if !found {
		t.Fatalf("реакция не попала в историю: %+v", hist)
	}
}
