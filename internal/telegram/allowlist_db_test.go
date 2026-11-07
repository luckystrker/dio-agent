//go:build db

package telegram

import (
	"context"
	"testing"
)

// Whitelist лички в БД: чужая личка — journal_only и не снимает пометку
// блокировки; на старте существующие чужие лички помечаются
// заблокированными с отменой их намерений.
func TestPrivateAllowlistIngestAndStartupBlock(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const friend, stranger, group = int64(722958928), int64(42), int64(-100500)
	allow := NewPrivateAllowlist([]int64{friend})

	// История до включения whitelist: обе лички и группа писали боту.
	for i, chatID := range []int64{friend, stranger} {
		mustIngest(t, pool, rawMessageOf(int64(100+i), chatID, "привет"))
	}
	mustIngest(t, pool, rawGroupMessageOf(110, group, "всем привет"))

	var provenanceID, intentionID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO provenance (kind, ref, scopes) VALUES ('runtime_action', 'test:allow', ARRAY['chat:42']) RETURNING id`).
		Scan(&provenanceID); err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO intentions (kind, target_chat_id, topic, reason, provenance_id, dedupe_key)
		VALUES ('check_in', $1, 'как дела', 'тест whitelist', $2, 'test-allow') RETURNING id`, stranger, provenanceID).
		Scan(&intentionID); err != nil {
		t.Fatalf("намерение: %v", err)
	}

	n, err := BlockForeignPrivateChats(ctx, pool, allow)
	if err != nil {
		t.Fatalf("стартовая пометка: %v", err)
	}
	if n != 1 {
		t.Fatalf("помечено чатов %d, хочу 1 (только чужая личка)", n)
	}
	for chatID, want := range map[int64]bool{friend: false, stranger: true, group: false} {
		if blocked, _ := blockedChatState(t, pool, chatID, 0); blocked != want {
			t.Fatalf("чат %d: blocked=%v, хочу %v", chatID, blocked, want)
		}
	}
	if _, status := blockedChatState(t, pool, stranger, intentionID); status != "cancelled" {
		t.Fatalf("намерение чужой лички: %s, хочу cancelled", status)
	}

	// Новое сообщение чужого: journal_only и пометка остаётся.
	res, err := IngestUpdateFiltered(ctx, pool, rawMessageOf(200, stranger, "ау"), BotIdentity{}, allow)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Status != StatusJournalOnly {
		t.Fatalf("чужая личка: статус %s, хочу journal_only", res.Status)
	}
	if blocked, _ := blockedChatState(t, pool, stranger, 0); !blocked {
		t.Fatalf("сообщение чужого не должно снимать пометку блокировки")
	}
	// Своя личка — в ход.
	res, err = IngestUpdateFiltered(ctx, pool, rawMessageOf(201, friend, "ау"), BotIdentity{}, allow)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Status != StatusPending {
		t.Fatalf("своя личка: статус %s, хочу pending", res.Status)
	}
}
