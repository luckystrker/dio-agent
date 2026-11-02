//go:build db

// Доп-проверка 18.2 к TestRecentHistoryExcludesPendingUserEvents: фильтр бьёт
// точно по pending и не выкидывает из недавней истории низкосигнальный шум
// группы (journal_only) — §6.3 «низкосигнальные записи доступны в недавней
// истории».
package character

import (
	"testing"
)

func TestRecentHistoryKeepsJournalOnly(t *testing.T) {
	f, ctx := newFixture(t)
	pool := f.pool
	chat := testChatID

	seed := func(updateID int64, status string, turnID any, text string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, kind, text, raw_update, status, turn_id)
			VALUES ($1, $2, 'group', 777, 'message', $3, '{}'::jsonb, $4, $5)`,
			updateID, chat, text, status, turnID); err != nil {
			t.Fatalf("событие %d не вставлено: %v", updateID, err)
		}
	}
	seed(20, "pending", nil, "ещё в буфере")
	seed(21, "journal_only", nil, "пассивный шум группы")
	seed(22, "processed", 44, "закрытая реплика")

	history, err := RecentHistory(ctx, pool, chat, 1000, 0)
	if err != nil {
		t.Fatalf("история не читается: %v", err)
	}
	has := func(text string) bool {
		for _, m := range history {
			if m.Content == text {
				return true
			}
		}
		return false
	}
	if has("ещё в буфере") {
		t.Fatalf("pending просочился в историю (18.2): %+v", history)
	}
	if !has("пассивный шум группы") {
		t.Fatalf("journal_only пропал из недавней истории — фильтр бьёт шире pending (§6.3): %+v", history)
	}
	if !has("закрытая реплика") {
		t.Fatalf("processed-событие пропало из истории: %+v", history)
	}
}
