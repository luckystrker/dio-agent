// Тесты этапа 25.4–25.5: поле стикера в TurnResult (метка коллекции) и
// предложение сохранить присланный стикер. Валидация — как у реакции:
// мусорная метка при answer мягко обнуляется runtime, слишком длинная ловится
// до применения.
package character

import (
	"strings"
	"testing"
)

// TestParseTurnResultSticker — sticker принимается вместе с answer/react,
// save_sticker — объект с заметкой.
func TestParseTurnResultSticker(t *testing.T) {
	raw := []byte(`{"decision":"answer","messages":["ну держи"],"sticker":"#3"}`)
	r, err := ParseTurnResult(raw)
	if err != nil {
		t.Fatalf("валидный стикер не разобран: %v", err)
	}
	if r.Sticker != "#3" {
		t.Fatalf("стикер = %q, хочу #3", r.Sticker)
	}

	raw = []byte(`{"decision":"react","reaction":"🤣","sticker":"#7"}`)
	r, err = ParseTurnResult(raw)
	if err != nil {
		t.Fatalf("react со стикером не разобран: %v", err)
	}
	if r.Sticker != "#7" {
		t.Fatalf("стикер при react = %q, хочу #7", r.Sticker)
	}

	raw = []byte(`{"decision":"ignore","save_sticker":{"note":"когда жарко"}}`)
	r, err = ParseTurnResult(raw)
	if err != nil {
		t.Fatalf("save_sticker не разобран: %v", err)
	}
	if r.SaveSticker == nil || r.SaveSticker.Note != "когда жарко" {
		t.Fatalf("save_sticker потерян: %+v", r.SaveSticker)
	}
}

// TestTurnResultStickerRules — потолок метки и заметки; при ignore/defer
// стикер не отправляется (поле без эффекта очищается, как мусорная реакция).
func TestTurnResultStickerRules(t *testing.T) {
	long := strings.Repeat("9", maxStickerLabel+1)
	if err := (TurnResult{Decision: DecisionAnswer, Messages: []string{"x"}, Sticker: long}).Validate(); err == nil {
		t.Fatal("слишком длинная метка прошла валидацию")
	}
	if err := (TurnResult{Decision: DecisionAnswer, Messages: []string{"x"},
		SaveSticker: &SaveStickerProposal{Note: strings.Repeat("я", stickerNoteLimit+1)}}).Validate(); err == nil {
		t.Fatal("слишком длинная заметка сохранения прошла валидацию")
	}
	if err := (TurnResult{Decision: DecisionAnswer, Messages: []string{"x"},
		SaveSticker: &SaveStickerProposal{}}).Validate(); err == nil {
		t.Fatal("пустая заметка сохранения прошла валидацию")
	}

	// ignore с мусорным стикером: поле молча обнуляется до Validate.
	r, err := ParseTurnResult([]byte(`{"decision":"ignore","sticker":"#3"}`))
	if err != nil {
		t.Fatalf("ignore со стикером упал: %v", err)
	}
	if r.Sticker != "" {
		t.Fatalf("ignore отправил бы стикер: %q", r.Sticker)
	}
	// defer — то же самое.
	r, err = ParseTurnResult([]byte(`{"decision":"defer","defer_hours":2,"sticker":"#3"}`))
	if err != nil {
		t.Fatalf("defer со стикером упал: %v", err)
	}
	if r.Sticker != "" {
		t.Fatalf("defer отправил бы стикер: %q", r.Sticker)
	}
}

// TestTurnResultSchemaSticker — схема провода знает sticker и save_sticker
// (иначе модель о них не узнает вообще). С 28.1 описания полей говорят, что
// стикер — редкая приправа, а save_sticker уместен только для незнакомого
// стикера пачки: модель не предлагает сохранение в каждом ходе.
func TestTurnResultSchemaSticker(t *testing.T) {
	props := TurnResultJSONSchema()["properties"].(map[string]any)
	sticker, ok := props["sticker"].(map[string]any)
	if !ok {
		t.Fatalf("в схеме нет поля sticker")
	}
	if sticker["maxLength"] != maxStickerLabel {
		t.Fatalf("схема: maxLength sticker = %v, валидатор — %d", sticker["maxLength"], maxStickerLabel)
	}
	if desc, _ := sticker["description"].(string); !strings.Contains(desc, "редкая") {
		t.Fatalf("описание sticker не говорит, что стикер — редкая приправа: %q", desc)
	}
	save, ok := props["save_sticker"].(map[string]any)
	if !ok {
		t.Fatalf("в схеме нет поля save_sticker")
	}
	if desc, _ := save["description"].(string); !strings.Contains(desc, "НЕТ в твоей коллекции") {
		t.Fatalf("описание save_sticker не ограничивает незнакомым стикером пачки: %q", desc)
	}
	note := save["properties"].(map[string]any)["note"].(map[string]any)
	if note["maxLength"] != stickerNoteLimit {
		t.Fatalf("схема: maxLength заметки = %v, валидатор — %d", note["maxLength"], stickerNoteLimit)
	}
}

// TestConstraintsStickerPolicy — 28.1: runtime-ограничения хода прямо говорят
// «стикер редко» и «save_sticker только для незнакомого стикера пачки».
func TestConstraintsStickerPolicy(t *testing.T) {
	text := constraintsSection()
	for _, want := range []string{"редкая приправа", "которого НЕТ в твоей коллекции", "отбросит"} {
		if !strings.Contains(text, want) {
			t.Fatalf("в constraints нет %q:\n%s", want, text)
		}
	}
}
