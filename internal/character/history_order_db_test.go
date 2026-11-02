//go:build db

// БД-тест порядка недавней истории (18.12): соседние реплики с равным
// временем из двух таблиц (UNION с разными часами occurred_at/created_at)
// обязаны идти в одном и том же порядке от запроса к запросу — иначе окно
// между ходами меняется и ломает кэш префикса провайдера. Порядок задаётся
// полностью: ts DESC, id DESC, роль — финальный тай-брейк двух независимых
// последовательностей id.
package character

import (
	"context"
	"testing"
	"time"
)

func TestRecentHistoryDeterministicOrderOnEqualTs(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	chat := testChatID

	// Одна секунда на все пять реплик: порядок решает только тай-брейк.
	sameTS := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	seedUser := func(updateID int64, text string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status, occurred_at)
			VALUES ($1, $2, 'group', 777, 'message', $3, '{}'::jsonb, 'processed', $4)`,
			updateID, chat, text, sameTS); err != nil {
			t.Fatalf("событие %d не вставлено: %v", updateID, err)
		}
	}
	seedAssistant := func(position int, text string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, created_at)
			VALUES (9, $1, $2, $3::jsonb, 'sent', $4)`,
			position, chat, `{"text":"`+text+`"}`, sameTS); err != nil {
			t.Fatalf("ответ %d не вставлен: %v", position, err)
		}
	}

	// id по последовательностям: user 1..3, assistant 1..2; при равном ts
	// свежее — больший id, при равном id сначала user (роль по убыванию).
	seedUser(101, "u1")
	seedUser(102, "u2")
	seedUser(103, "u3")
	seedAssistant(1, "a1")
	seedAssistant(2, "a2")

	texts := func(history []HistoryMessage) []string {
		out := make([]string, 0, len(history))
		for _, m := range history {
			out = append(out, m.Content)
		}
		return out
	}

	// Полное окно: порядок одинаков от запроса к запросу.
	wantAll := []string{"a1", "u1", "a2", "u2", "u3"}
	for round := 0; round < 5; round++ {
		history, err := RecentHistory(ctx, pool, chat, 10, 0)
		if err != nil {
			t.Fatalf("история не читается (раунд %d): %v", round, err)
		}
		if got := texts(history); !equalStrings(got, wantAll) {
			t.Fatalf("порядок при равных ts нестабилен (раунд %d): %v, хочу %v", round, got, wantAll)
		}
	}

	// Усечённое окно: граница LIMIT тоже детерминирована — в окно попадают
	// одни и те же строки.
	history, err := RecentHistory(ctx, pool, chat, 3, 0)
	if err != nil {
		t.Fatalf("история не читается: %v", err)
	}
	if got := texts(history); !equalStrings(got, []string{"a2", "u2", "u3"}) {
		t.Fatalf("окно LIMIT 3 нестабильно: %v", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
