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

// M2 ревью 2026-11-07: возврат id в TELEGRAM_PRIVATE_ALLOWLIST снимает на
// старте пометку, поставленную самим whitelist'ом; настоящая блокировка
// пользователем (другой reason) остаётся.
func TestAllowedPrivateChatUnblockedOnStartup(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const friend, stranger, blockedByUser = int64(722958928), int64(42), int64(555)

	mustIngest(t, pool, rawMessageOf(600, stranger, "привет"))
	// Пока stranger вне списка — старт помечает его (как 42.1).
	if n, err := BlockForeignPrivateChats(ctx, pool, NewPrivateAllowlist([]int64{friend})); err != nil || n != 1 {
		t.Fatalf("стартовая пометка: n=%d err=%v, хочу 1", n, err)
	}
	// Настоящая блокировка от Telegram — не трогается, даже если id снова
	// в списке (проверяем именно предикат reason, а не фильтр chat_id).
	if _, err := MarkChatBlocked(ctx, pool, blockedByUser, "Forbidden: bot was blocked by the user"); err != nil {
		t.Fatalf("настоящая блокировка: %v", err)
	}

	n, err := UnblockAllowedPrivateChats(ctx, pool, NewPrivateAllowlist([]int64{friend, stranger, blockedByUser}))
	if err != nil {
		t.Fatalf("разблокировка разрешённых: %v", err)
	}
	if n != 1 {
		t.Fatalf("снято пометок %d, хочу 1 (только вернувшийся в список)", n)
	}
	if blocked, _ := blockedChatState(t, pool, stranger, 0); blocked {
		t.Fatalf("id вернулся в список — пометка whitelist должна быть снята")
	}
	if blocked, _ := blockedChatState(t, pool, blockedByUser, 0); !blocked {
		t.Fatalf("настоящая блокировка пользователем не должна сниматься")
	}
}

// m4 ревью 2026-11-07: правка и реакция чужой лички — journal_only, offset
// двигается; дубль update хода не создаёт и offset не откатывает.
func TestPrivateAllowlistIngestEditedAndReaction(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const friend, stranger = int64(722958928), int64(42)
	allow := NewPrivateAllowlist([]int64{friend})

	mustIngest(t, pool, rawMessageOf(700, stranger, "привет"))
	edit, err := IngestUpdateFiltered(ctx, pool, rawEditOf(701, 700, stranger, "привет!"), BotIdentity{}, allow)
	if err != nil {
		t.Fatalf("правка чужой лички: %v", err)
	}
	if edit.Event.Kind != KindEdited || edit.Status != StatusJournalOnly {
		t.Fatalf("правка чужой лички: kind=%s status=%s, хочу edited/journal_only", edit.Event.Kind, edit.Status)
	}
	if offset, has, err := ReadOffset(ctx, pool); err != nil || !has || offset != 702 {
		t.Fatalf("offset после правки = %d (has=%v err=%v), хочу 702", offset, has, err)
	}

	// Реакция чужого на ответ бота в его же личке: без whitelist она была бы
	// pending (цель найдена), с whitelist — только журнал.
	insertSentOutbox(t, ctx, pool, stranger, 77, "message", "ответ бота", "")
	reaction, err := IngestUpdateFiltered(ctx, pool, rawReactionOf(702, stranger, 77, "", "😂"), BotIdentity{}, allow)
	if err != nil {
		t.Fatalf("реакция чужой лички: %v", err)
	}
	if reaction.Status != StatusJournalOnly {
		t.Fatalf("реакция чужой лички: статус %s, хочу journal_only", reaction.Status)
	}
	if offset, has, err := ReadOffset(ctx, pool); err != nil || !has || offset != 703 {
		t.Fatalf("offset после реакции = %d (has=%v err=%v), хочу 703", offset, has, err)
	}

	// Снятие реакции чужого — тоже только журнал (41.3 m28: снятие не несёт
	// адресованного обращения); whitelist не должен превращать его в ход.
	removal, err := IngestUpdateFiltered(ctx, pool, rawReactionOf(703, stranger, 77, "😂", ""), BotIdentity{}, allow)
	if err != nil {
		t.Fatalf("снятие реакции чужой лички: %v", err)
	}
	if removal.Status != StatusJournalOnly || removal.Event.ReactionAdded {
		t.Fatalf("снятие реакции чужой лички: статус %s added=%v, хочу journal_only/false",
			removal.Status, removal.Event.ReactionAdded)
	}
	if offset, has, err := ReadOffset(ctx, pool); err != nil || !has || offset != 704 {
		t.Fatalf("offset после снятия реакции = %d (has=%v err=%v), хочу 704", offset, has, err)
	}
}

// Находка тестировщика 2026-11-07 (M2): доставка помечает чат причиной
// sendErr.Error() (delivery.go: message := sendErr.Error()), то есть
// "sendMessage: HTTP 403 — Forbidden: private chat is not in allowlist", а не
// голым allowlistDenied. UnblockAllowedPrivateChats ищет точное равенство —
// пометка, поставленная доставкой (осиротевшее намерение без social_events,
// m1), при возврате id в список не снимается. Ожидание — любая пометка,
// порождённая самим whitelist'ом, снимается; настоящие блокировки Telegram
// (другой текст) остаются.
func TestRelistedChatUnblocksDeliveryDenial(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const friend = int64(722958928)

	inner := &recordingAPI{}
	guarded := GuardAPI(inner, NewPrivateAllowlist([]int64{999999}))
	_, sendErr := guarded.SendMessage(ctx, friend, "в чужую личку", 0)
	if sendErr == nil || !ChatBlockedError(sendErr) {
		t.Fatalf("GuardAPI должен вернуть ChatBlockedError, получено %v", sendErr)
	}
	if _, err := MarkChatBlocked(ctx, pool, friend, sendErr.Error()); err != nil {
		t.Fatalf("пометка доставки: %v", err)
	}

	n, err := UnblockAllowedPrivateChats(ctx, pool, NewPrivateAllowlist([]int64{friend}))
	if err != nil {
		t.Fatalf("разблокировка разрешённых: %v", err)
	}
	if n != 1 {
		t.Fatalf("пометка доставки (reason=%q) не снята при возврате id в список: снято %d, хочу 1",
			sendErr.Error(), n)
	}
	if blocked, _ := blockedChatState(t, pool, friend, 0); blocked {
		t.Fatalf("id вернулся в список — пометка whitelist от доставки должна быть снята")
	}
}

func TestPrivateAllowlistDuplicateUpdateDoesNotRollbackOffset(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const friend, stranger = int64(722958928), int64(42)
	allow := NewPrivateAllowlist([]int64{friend})

	raw := rawMessageOf(800, stranger, "ау")
	first, err := IngestUpdateFiltered(ctx, pool, raw, BotIdentity{}, allow)
	if err != nil || !first.Inserted || first.Status != StatusJournalOnly {
		t.Fatalf("первое чужое: inserted=%v status=%s err=%v", first.Inserted, first.Status, err)
	}
	// Свой ход двигает offset дальше.
	if _, err := IngestUpdateFiltered(ctx, pool, rawMessageOf(810, friend, "привет"), BotIdentity{}, allow); err != nil {
		t.Fatalf("своё сообщение: %v", err)
	}
	if offset, _, _ := ReadOffset(ctx, pool); offset != 811 {
		t.Fatalf("offset перед дублем = %d, хочу 811", offset)
	}

	dup, err := IngestUpdateFiltered(ctx, pool, raw, BotIdentity{}, allow)
	if err != nil {
		t.Fatalf("дубль чужого: %v", err)
	}
	if dup.Inserted {
		t.Fatalf("дубль записан второй раз")
	}
	if dup.Status != StatusJournalOnly {
		t.Fatalf("дубль чужого: статус %s, хочу journal_only (хода нет)", dup.Status)
	}
	if offset, has, err := ReadOffset(ctx, pool); err != nil || !has || offset != 811 {
		t.Fatalf("дубль откатил offset: %d (has=%v err=%v), хочу 811", offset, has, err)
	}
	var events, turns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM social_events WHERE telegram_update_id = 800`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("событий по update 800: %d (err=%v), хочу 1", events, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE chat_id = $1`, stranger).Scan(&turns); err != nil || turns != 0 {
		t.Fatalf("ходов по чужой личке: %d (err=%v), хочу 0", turns, err)
	}
}
