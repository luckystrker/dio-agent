//go:build db

// БД-тесты описаний картинок (этап 37.1): применение кладёт описание в
// событие пачки (и не создаёт память), история показывает его вместо
// плейсхолдера; отправленная ботом картинка вспоминается с alt/доменом.
package character

import (
	"testing"
)

func TestApplyTurnStoresImageNotesDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3711)
	f.seedTurn(t, ctx, turnID, "смотри что нашёл")

	// Второе событие пачки — стикер: у него своё описание.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, turn_id, telegram_message_id)
		VALUES (3711002, $1, 'private', 777, 'Коля', 'sticker', '[стикер 😂]', '{"message":{"message_id":5002}}'::jsonb, 'buffered', $2, 5002)`,
		testChatID, turnID); err != nil {
		t.Fatalf("событие-стикер не вставлено: %v", err)
	}
	var photoEventID, stickerEventID int64
	if err := f.pool.QueryRow(ctx, `
		SELECT id FROM social_events WHERE turn_id = $1 AND kind = 'message'`, turnID).Scan(&photoEventID); err != nil {
		t.Fatalf("событие фото не найдено: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT id FROM social_events WHERE turn_id = $1 AND kind = 'sticker'`, turnID).Scan(&stickerEventID); err != nil {
		t.Fatalf("событие стикера не найдено: %v", err)
	}
	var memoriesBefore int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM memory_items`).Scan(&memoriesBefore); err != nil {
		t.Fatalf("память не прочитана: %v", err)
	}

	res, err := ApplyTurn(ctx, f.pool, ApplyOptions{
		TurnID:  turnID,
		ChatID:  testChatID,
		ScopeID: testScopeID,
		Result:  TurnResult{Decision: DecisionAnswer, Messages: []string{"красиво"}},
		ImageNotes: []EventImageNote{
			{EventID: photoEventID, Kind: imageKindPhoto, Note: "закат над морем"},
			{EventID: stickerEventID, Kind: imageKindSticker, Note: "кот смеётся"},
		},
		PersonaBaseline: f.persona.MoodBaseline,
		Journal:         f.journal,
	})
	if err != nil {
		t.Fatalf("применение: %v", err)
	}
	if !res.Applied {
		t.Fatal("ход не применён")
	}

	var note, kind string
	if err := f.pool.QueryRow(ctx, `
		SELECT image_note, image_kind FROM social_events WHERE id = $1`, photoEventID).Scan(&note, &kind); err != nil {
		t.Fatalf("описание фото не прочитано: %v", err)
	}
	if note != "закат над морем" || kind != imageKindPhoto {
		t.Fatalf("описание фото = %q/%q", note, kind)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT image_note, image_kind FROM social_events WHERE id = $1`, stickerEventID).Scan(&note, &kind); err != nil {
		t.Fatalf("описание стикера не прочитано: %v", err)
	}
	if note != "кот смеётся" || kind != imageKindSticker {
		t.Fatalf("описание стикера = %q/%q", note, kind)
	}

	// Описания — история, не память (§37.1): memory_items не выросла.
	var memoriesAfter int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM memory_items`).Scan(&memoriesAfter); err != nil {
		t.Fatalf("память не прочитана: %v", err)
	}
	if memoriesAfter != memoriesBefore {
		t.Fatalf("описания создали память: было %d, стало %d", memoriesBefore, memoriesAfter)
	}
}

func TestRecentHistoryShowsImageNotesDB(t *testing.T) {
	f, ctx := newFixture(t)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, telegram_message_id, image_note, image_kind, occurred_at)
		VALUES
		  (3721001, $1, 'private', 777, 'Коля', 'photo', '', '{}'::jsonb, 'processed', 6001, 'закат над морем', 'photo', now() - interval '6 minutes'),
		  (3721002, $1, 'private', 777, 'Коля', 'sticker', '[стикер 😂]', '{}'::jsonb, 'processed', 6002, 'кот смеётся', 'sticker', now() - interval '5 minutes'),
		  (3721003, $1, 'private', 777, 'Коля', 'message', '[гифка] прыжок', '{}'::jsonb, 'processed', 6003, 'пёс прыгает', 'gif', now() - interval '4 minutes'),
		  (3721004, $1, 'private', 777, 'Коля', 'message', '[кружок, 0:15]', '{}'::jsonb, 'processed', 6004, 'человек машет', 'video_note', now() - interval '3 minutes')`,
		testChatID); err != nil {
		t.Fatalf("события с описаниями не вставлены: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, telegram_message_id, created_at)
		VALUES
		  (3722001, 1, $1, '{"kind":"photo","source_url":"https://cdn.example.com/a.jpg","alt":"Кот в коробке"}'::jsonb, 'sent', 7001, now() - interval '2 minutes'),
		  (3722002, 1, $1, '{"kind":"photo","source_url":"https://images.example.org/b.png"}'::jsonb, 'sent', 7002, now() - interval '1 minute')`,
		testChatID); err != nil {
		t.Fatalf("отправленные картинки не вставлены: %v", err)
	}

	history, _, err := RecentHistoryWindow(ctx, f.pool, testChatID, 10, 0)
	if err != nil {
		t.Fatalf("история не прочитана: %v", err)
	}
	want := []string{
		"[фото: закат над морем]",
		"[стикер 😂: кот смеётся]",
		"[гифка: пёс прыгает] прыжок",
		"[кружок 0:15: человек машет]",
		"[отправил картинку: Кот в коробке]",
		"[отправил картинку: images.example.org]",
	}
	if len(history) != len(want) {
		t.Fatalf("история: %d реплик, хочу %d: %+v", len(history), len(want), history)
	}
	for i, expected := range want {
		if history[i].Content != expected {
			t.Fatalf("реплика[%d] = %q, хочу %q", i, history[i].Content, expected)
		}
	}
	if history[0].Label != "#m6001" || history[4].Label != "#m7001" {
		t.Fatalf("метки истории = %q/%q", history[0].Label, history[4].Label)
	}
}

// TestRecentHistoryWithoutNoteUnchangedDB — нет описания — прежний вид:
// подпись фото/плейсхолдер не подменяются.
func TestRecentHistoryWithoutNoteUnchangedDB(t *testing.T) {
	f, ctx := newFixture(t)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, telegram_message_id)
		VALUES
		  (3731001, $1, 'private', 777, 'Коля', 'photo', '', '{}'::jsonb, 'processed', 6101),
		  (3731002, $1, 'private', 777, 'Коля', 'photo', 'подпись', '{}'::jsonb, 'processed', 6102)`,
		testChatID); err != nil {
		t.Fatalf("события не вставлены: %v", err)
	}
	history, _, err := RecentHistoryWindow(ctx, f.pool, testChatID, 10, 0)
	if err != nil {
		t.Fatalf("история не прочитана: %v", err)
	}
	if len(history) != 2 || history[0].Content != "[фото]" || history[1].Content != "подпись" {
		t.Fatalf("история без описаний изменилась: %+v", history)
	}
}
