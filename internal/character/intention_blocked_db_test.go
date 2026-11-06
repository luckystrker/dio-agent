//go:build db

// БД-тест этапа 38.7: применение хода не создаёт новых инициатив в чат,
// который заблокировал бота, — даже если модель их предложила. Пометка живёт
// в blocked_chats, гейт стоит в единственной точке создания намерений.
package character

import (
	"testing"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

func TestCreateIntentionSkipsBlockedChat(t *testing.T) {
	f, ctx := newFixture(t)
	if _, err := telegram.MarkChatBlocked(ctx, f.pool, testChatID, "Forbidden: bot was blocked by the user"); err != nil {
		t.Fatalf("чат не помечен: %v", err)
	}

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("транзакция не открыта: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := intentionRow{
		kind: "check_in", chatID: testChatID, scopeID: testScopeID,
		topic: "как дела", reason: "тест блокировки", delayHours: 0.5,
	}
	id, created, dup, err := createIntention(ctx, tx, row)
	if err != nil {
		t.Fatalf("создание намерения упало: %v", err)
	}
	if created || id != 0 {
		t.Fatalf("намерение в заблокированный чат создано (id=%d created=%v)", id, created)
	}
	if dup == nil || dup.LiveStatus != "blocked" {
		t.Fatalf("причина отказа не «заблокирован»: %+v", dup)
	}

	// Пометка снята — намерение снова создаётся на общем пути.
	if err := telegram.UnblockChat(ctx, f.pool, testChatID); err != nil {
		t.Fatalf("пометка не снята: %v", err)
	}
	row.reason = "тест после разблокировки"
	id, created, dup, err = createIntention(ctx, tx, row)
	if err != nil || !created || id == 0 || dup != nil {
		t.Fatalf("после снятия пометки намерение не создано: id=%d created=%v dup=%+v err=%v", id, created, dup, err)
	}
}
