//go:build db

// БД-тесты этапа 25.4–25.5: стикер из коллекции уезжает в outbox после
// сообщений/реакции, неизвестная метка мягко обнуляется, предложение
// «сохраню себе» сохраняет стикер из пачки и не плодит дубли.
package character

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/luckystrker/ago/events"
)

// seedStickerEvent — событие пачки: присланный стикер (kind=sticker).
func (f *fixture) seedStickerEvent(t *testing.T, ctx context.Context, turnID int64, raw string) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, turn_id, telegram_message_id)
		VALUES ($1, $2, 'private', 777, 'Коля', 'sticker', '[стикер 😂]', $3::jsonb, 'buffered', $4, 9401)`,
		2_000_000+turnID, testChatID, raw, turnID); err != nil {
		t.Fatalf("событие-стикер не вставлено: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO turns (id, chat_id, status) VALUES ($1, $2, 'created')`, turnID, testChatID); err != nil {
		t.Fatalf("turn не вставлен: %v", err)
	}
}

// outboxRows — payload'ы доставки хода по порядку позиций.
func (f *fixture) outboxRows(t *testing.T, ctx context.Context, turnID int64) []map[string]any {
	t.Helper()
	rows, err := f.pool.Query(ctx,
		`SELECT payload FROM delivery_outbox WHERE turn_id = $1 ORDER BY position`, turnID)
	if err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("строка outbox не читается: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("payload не разобран: %v", err)
		}
		out = append(out, payload)
	}
	return out
}

// TestApplySendsStickerAfterMessages — answer со стикером: сначала сообщения,
// затем стикер с file_id из коллекции (модель метку придумать не может).
func TestApplySendsStickerAfterMessages(t *testing.T) {
	f, ctx := newFixture(t)
	if _, err := NewStickerRepo().SyncPersona(ctx, f.pool, []SourceSticker{
		{FileID: "FILE-1", Emoji: "😂", Note: "ржу"},
	}); err != nil {
		t.Fatalf("коллекция не создана: %v", err)
	}
	list, err := NewStickerRepo().List(ctx, f.pool, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("коллекция: %v %v", list, err)
	}
	f.seedTurn(t, ctx, 401, "поржали?")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer",` +
		`"messages":["ну"],"sticker":"` + stickerLabel(list[0].ID) + `"}`}})

	out, err := f.runner.Run(ctx, Input{TurnID: 401, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "поржали?"}}})
	if err != nil || !out.Apply.Applied || out.Apply.SkippedSticker {
		t.Fatalf("ход со стикером: out=%+v err=%v", out.Apply, err)
	}
	rows := f.outboxRows(t, ctx, 401)
	if len(rows) != 2 {
		t.Fatalf("строк доставки %d, хочу 2 (сообщение + стикер): %v", len(rows), rows)
	}
	if rows[0]["kind"] != "message" || rows[0]["text"] != "ну" {
		t.Fatalf("первая строка не сообщение: %v", rows[0])
	}
	if rows[1]["kind"] != "sticker" || rows[1]["file_id"] != "FILE-1" {
		t.Fatalf("вторая строка не стикер коллекции: %v", rows[1])
	}
}

// TestApplyUnknownStickerLabelSoftlyDropped — метка не из панели: стикера нет,
// ход живёт (как мусорная reaction при answer).
func TestApplyUnknownStickerLabelSoftlyDropped(t *testing.T) {
	f, ctx := newFixture(t)
	f.seedTurn(t, ctx, 402, "ну?")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer",` +
		`"messages":["ну"],"sticker":"#99999"}`}})

	out, err := f.runner.Run(ctx, Input{TurnID: 402, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "ну?"}}})
	if err != nil || !out.Apply.Applied {
		t.Fatalf("ход не применился: %+v err=%v", out.Apply, err)
	}
	if !out.Apply.SkippedSticker {
		t.Fatal("неизвестная метка не помечена как пропущенная")
	}
	rows := f.outboxRows(t, ctx, 402)
	if len(rows) != 1 || rows[0]["kind"] != "message" {
		t.Fatalf("неизвестная метка отправила лишнее: %v", rows)
	}
}

// TestApplyReactWithSticker — стикер работает и при decision=react: реакция,
// затем стикер.
func TestApplyReactWithSticker(t *testing.T) {
	f, ctx := newFixture(t)
	if _, err := NewStickerRepo().SyncPersona(ctx, f.pool, []SourceSticker{{FileID: "FILE-R", Emoji: "🔥"}}); err != nil {
		t.Fatalf("коллекция: %v", err)
	}
	list, _ := NewStickerRepo().List(ctx, f.pool, 5)
	f.seedTurn(t, ctx, 403, "огонь")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"react",` +
		`"reaction":"🔥","sticker":"` + stickerLabel(list[0].ID) + `"}`}})

	out, err := f.runner.Run(ctx, Input{TurnID: 403, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "огонь"}}})
	if err != nil || !out.Apply.Applied {
		t.Fatalf("react со стикером: out=%+v err=%v", out.Apply, err)
	}
	rows := f.outboxRows(t, ctx, 403)
	if len(rows) != 2 || rows[0]["kind"] != "reaction" || rows[1]["kind"] != "sticker" {
		t.Fatalf("порядок доставки react+стикер: %v", rows)
	}
}

// TestApplySavesStickerFromBatch — «сохраню себе»: file_id/уникальный id берутся
// из пачки, заметка — у модели; повтор не создаёт дубль (25.5).
func TestApplySavesStickerFromBatch(t *testing.T) {
	f, ctx := newFixture(t)
	raw := `{"message":{"message_id":9401,"sticker":{"file_id":"NEW-FILE","file_unique_id":"NEW-UNIQ","emoji":"🤣","set_name":"pack"},"from":{"id":777}}}`
	f.seedStickerEvent(t, ctx, 501, raw)
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"ignore",` +
		`"save_sticker":{"note":"когда кто-то тупит"}}`}})

	out, err := f.runner.Run(ctx, Input{TurnID: 501, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "[стикер 🤣]",
			Raw: []byte(raw)}}})
	if err != nil || !out.Apply.Applied {
		t.Fatalf("ход сохранения: out=%+v err=%v", out.Apply, err)
	}
	if out.Apply.SavedStickerID == 0 {
		t.Fatal("стикер не сохранён")
	}
	var (
		fileID, unique, emoji, note, source string
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT file_id, file_unique_id, emoji, note, source FROM stickers WHERE id = $1`,
		out.Apply.SavedStickerID).Scan(&fileID, &unique, &emoji, &note, &source); err != nil {
		t.Fatalf("сохранённый стикер не читается: %v", err)
	}
	if fileID != "NEW-FILE" || unique != "NEW-UNIQ" || emoji != "🤣" || note != "когда кто-то тупит" || source != "saved" {
		t.Fatalf("поля сохранённого стикера: %s/%s/%s/%s/%s", fileID, unique, emoji, note, source)
	}

	// Второй такой же ход: дубль по file_unique_id отсечён.
	f.seedStickerEvent(t, ctx, 502, raw)
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"ignore",` +
		`"save_sticker":{"note":"снова"}}`}})
	out, err = f.runner.Run(ctx, Input{TurnID: 502, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 2, ActorID: 777, ActorName: "Коля", Text: "[стикер 🤣]", Raw: []byte(raw)}}})
	if err != nil || !out.Apply.Applied {
		t.Fatalf("второй ход сохранения: out=%+v err=%v", out.Apply, err)
	}
	if out.Apply.SavedStickerID != 0 {
		t.Fatalf("дубль сохранён повторно: id=%d", out.Apply.SavedStickerID)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM stickers WHERE file_unique_id = 'NEW-UNIQ'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("строк с NEW-UNIQ: %d err=%v", count, err)
	}

	// Предложение без стикера в пачке: сохранять нечего, ход живёт.
	f.seedTurn(t, ctx, 503, "просто текст")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"ignore",` +
		`"save_sticker":{"note":"непонятно что"}}`}})
	out, err = f.runner.Run(ctx, Input{TurnID: 503, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 3, ActorID: 777, ActorName: "Коля", Text: "просто текст"}}})
	if err != nil || !out.Apply.Applied {
		t.Fatalf("ход без стикера: out=%+v err=%v", out.Apply, err)
	}
	if out.Apply.SavedStickerID != 0 {
		t.Fatalf("сохранён стикер, которого не присылали: id=%d", out.Apply.SavedStickerID)
	}
}
