//go:build db

// БД-тесты этапа 28.1: стикеры реже и по поводу. Живой прогон (ходы 121–128)
// показал стикер в 6 из 8 ходов и save_sticker в каждом ходе — даже там, где
// стикера в пачке не было. Runtime решает: предохранитель частоты, подсказка
// «можно сохранить» только для незнакомого стикера пачки и отброс save_sticker
// до записи, когда сохранять нечего.
package character

import (
	"context"
	"strings"
	"testing"

	"github.com/luckystrker/ago/events"
)

// priorTurn — прошлый ход чата без событий (нужен только id для окна частоты).
func (f *fixture) priorTurn(t *testing.T, ctx context.Context, chatID, turnID int64) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO turns (id, chat_id, status) VALUES ($1, $2, 'applied')`, turnID, chatID); err != nil {
		t.Fatalf("прошлый ход не вставлен: %v", err)
	}
}

func (f *fixture) queuedSticker(t *testing.T, ctx context.Context, chatID, turnID int64) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status)
		VALUES ($1, 0, $2, '{"kind":"sticker","file_id":"FILE-1"}'::jsonb, 'sent')`,
		turnID, chatID); err != nil {
		t.Fatalf("стикер прошлого хода не вставлен: %v", err)
	}
}

// TestApplyStickerFrequencyWindow — мягкий предохранитель частоты: стикер уже
// уходил в одном из последних ходов чата — предложение обнуляется, ход живёт;
// за пределами окна стикер снова разрешён.
func TestApplyStickerFrequencyWindow(t *testing.T) {
	f, ctx := newFixture(t)
	if _, err := NewStickerRepo().SyncPersona(ctx, f.pool, []SourceSticker{{FileID: "FILE-1", Emoji: "😂"}}); err != nil {
		t.Fatalf("коллекция: %v", err)
	}
	list, err := NewStickerRepo().List(ctx, f.pool, 5)
	if err != nil || len(list) != 1 {
		t.Fatalf("коллекция: %v %v", list, err)
	}
	label := stickerLabel(list[0].ID)

	f.priorTurn(t, ctx, testChatID, 801)
	f.queuedSticker(t, ctx, testChatID, 801)
	f.seedTurn(t, ctx, 802, "ну как?")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer",` +
		`"messages":["норм"],"sticker":"` + label + `"}`}})

	out, err := f.runner.Run(ctx, Input{TurnID: 802, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "ну как?"}}})
	if err != nil || !out.Apply.Applied {
		t.Fatalf("ход не применился: %+v err=%v", out.Apply, err)
	}
	if !out.Apply.SuppressedSticker {
		t.Fatal("стикер в окне частоты не обнулён")
	}
	rows := f.outboxRows(t, ctx, 802)
	if len(rows) != 1 || rows[0]["kind"] != "message" {
		t.Fatalf("обнулённый стикер всё равно ушёл: %v", rows)
	}

	// Окно: стикер был три хода назад — снова можно.
	f.priorTurn(t, ctx, testChatID, 803)
	f.priorTurn(t, ctx, testChatID, 804)
	f.seedTurn(t, ctx, 805, "и снова")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer",` +
		`"messages":["ага"],"sticker":"` + label + `"}`}})
	out, err = f.runner.Run(ctx, Input{TurnID: 805, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 2, ActorID: 777, ActorName: "Коля", Text: "и снова"}}})
	if err != nil || !out.Apply.Applied {
		t.Fatalf("ход за окном не применился: %+v err=%v", out.Apply, err)
	}
	if out.Apply.SuppressedSticker {
		t.Fatal("стикер за пределами окна частоты обнулён")
	}
	rows = f.outboxRows(t, ctx, 805)
	if len(rows) != 2 || rows[1]["kind"] != "sticker" {
		t.Fatalf("стикер за окном не ушёл: %v", rows)
	}
}

// TestApplyDropsSaveStickerForKnownBatchSticker — save_sticker при пачке, где
// все стикеры уже в коллекции, отбрасывается до записи: новой строки нет,
// признак отброса виден ходу; среди нескольких стикеров пачки сохраняется
// первый НЕзнакомый, а не первый вообще.
func TestApplyDropsSaveStickerForKnownBatchSticker(t *testing.T) {
	f, ctx := newFixture(t)
	if _, err := NewStickerRepo().SyncPersona(ctx, f.pool, []SourceSticker{{FileID: "KNOWN-FILE", Emoji: "😂"}}); err != nil {
		t.Fatalf("коллекция: %v", err)
	}
	knownRaw := `{"message":{"message_id":9501,"sticker":{"file_id":"KNOWN-FILE","file_unique_id":"KNOWN-UNIQ","emoji":"😂"},"from":{"id":777}}}`
	f.seedStickerEvent(t, ctx, 811, knownRaw)
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"ignore",` +
		`"save_sticker":{"note":"уже своё"}}`}})

	out, err := f.runner.Run(ctx, Input{TurnID: 811, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "[стикер 😂]", Raw: []byte(knownRaw)}}})
	if err != nil || !out.Apply.Applied {
		t.Fatalf("ход не применился: %+v err=%v", out.Apply, err)
	}
	if !out.Apply.DroppedStickerSave {
		t.Fatal("save_sticker по знакомому стикеру не помечен отброшенным")
	}
	if out.Apply.SavedStickerID != 0 {
		t.Fatalf("знакомый стикер сохранён повторно: id=%d", out.Apply.SavedStickerID)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM stickers`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("строк коллекции %d, хочу 1 (дубль не создан): err=%v", count, err)
	}

	// Два стикера в пачке: первый знакомый, второй новый — сохраняется второй.
	secondRaw := `{"message":{"message_id":9502,"sticker":{"file_id":"NEW-FILE","file_unique_id":"NEW-UNIQ","emoji":"🤣"},"from":{"id":777}}}`
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, turn_id, telegram_message_id)
		VALUES (9812, $1, 'private', 777, 'Коля', 'sticker', '[стикер 🤣]', $2::jsonb, 'buffered', 812, 9502)`,
		testChatID, secondRaw); err != nil {
		t.Fatalf("второй стикер пачки не вставлен: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO turns (id, chat_id, status) VALUES (812, $1, 'created')`, testChatID); err != nil {
		t.Fatalf("ход не вставлен: %v", err)
	}
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"ignore",` +
		`"save_sticker":{"note":"новый"}}`}})
	out, err = f.runner.Run(ctx, Input{TurnID: 812, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{
			{EventID: 2, ActorID: 777, ActorName: "Коля", Text: "[стикер 😂]", Raw: []byte(knownRaw)},
			{EventID: 3, ActorID: 777, ActorName: "Коля", Text: "[стикер 🤣]", Raw: []byte(secondRaw)},
		}})
	if err != nil || !out.Apply.Applied {
		t.Fatalf("ход со вторым стикером не применился: %+v err=%v", out.Apply, err)
	}
	if out.Apply.SavedStickerID == 0 || out.Apply.DroppedStickerSave {
		t.Fatalf("новый стикер пачки не сохранён: %+v", out.Apply)
	}
}

// TestPanelSaveStickerHintOnlyForUnknown — подсказка «можно сохранить» в панели
// печатается только тогда, когда в пачке есть стикер, которого нет в
// коллекции; в ходе без такого стикера модели нечего сохранять.
func TestPanelSaveStickerHintOnlyForUnknown(t *testing.T) {
	f, ctx := newFixture(t)
	f.runner.Assembler.Stickers = NewStickerRepo()
	if _, err := NewStickerRepo().SyncPersona(ctx, f.pool, []SourceSticker{{FileID: "KNOWN-FILE", Emoji: "😂"}}); err != nil {
		t.Fatalf("коллекция: %v", err)
	}
	knownRaw := []byte(`{"message":{"message_id":9601,"sticker":{"file_id":"KNOWN-FILE","emoji":"😂"},"from":{"id":777}}}`)
	unknownRaw := []byte(`{"message":{"message_id":9602,"sticker":{"file_id":"NEW-FILE","file_unique_id":"NEW-UNIQ","emoji":"🤣"},"from":{"id":777}}}`)
	hint := "можно сохранить"

	cases := []struct {
		name string
		raw  []byte
		want bool
	}{
		{"незнакомый стикер", unknownRaw, true},
		{"знакомый стикер", knownRaw, false},
		{"без стикера", nil, false},
	}
	for _, tc := range cases {
		batch := []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "привет", Raw: tc.raw}}
		bctx, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, batch, 0)
		if err != nil {
			t.Fatalf("%s: контекст не собран: %v", tc.name, err)
		}
		got := strings.Contains(bctx.StatePanel, hint)
		if got != tc.want {
			t.Fatalf("%s: подсказка=%v, хочу %v:\n%s", tc.name, got, tc.want, bctx.StatePanel)
		}
	}
}
