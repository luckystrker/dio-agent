// Юнит-тесты этапа 33.1: метка стикера в ответе модели — это номер из
// коллекции (#N) или эмодзи из неё; пустые/служебные заполнения («null»,
// «none», «нет», только пунктуация, «не используется…») — не запрос стикера
// и не ошибка, а «#4 🐱» — сначала номер, потом эмодзи.
package character

import (
	"strings"
	"testing"
)

// TestStickerLabelNone — служебные заполнения не считаются меткой.
func TestStickerLabelNone(t *testing.T) {
	for _, in := range []string{
		"", " ", "null", "NULL", "none", "NONE", "nil", "нет", "-", "—", "–",
		".", ",", ", ''", ",,}  } ,", "!", "...", "?:;",
		"не используется в этом ходу", "Не используется", "не нужен", "нет стикера",
	} {
		if !stickerLabelNone(in) {
			t.Errorf("%q: служебное заполнение принято за запрос стикера", in)
		}
	}
	for _, in := range []string{"#3", "3", "😎", "#4 🐱", "✅4", "4️⃣", "кот", "нет, поставь 😎"} {
		if stickerLabelNone(in) {
			t.Errorf("%q: настоящий запрос стикера отброшен как служебный", in)
		}
	}
}

// TestParseStickerRef — номер валиден раньше эмодзи; иначе первый
// эмодзи-кластер строки.
func TestParseStickerRef(t *testing.T) {
	idCases := map[string]int64{
		"#4 🐱":  4,
		"4 🐱":   4,
		"#4":    4,
		", #2.": 2,
	}
	for in, want := range idCases {
		ref, ok := parseStickerRef(in)
		if !ok || ref.ID != want || ref.Emoji != "" {
			t.Fatalf("%q: ref=%+v ok=%v, хочу номер %d", in, ref, ok, want)
		}
	}
	emojiCases := map[string]string{
		"😎":        "😎",
		"✅4":       "✅", // эмодзи первый — «4» уже не метка
		"4️⃣":      "4️⃣",
		"🐱 #4":     "🐱",
		"стикер 🐱": "🐱",
		"😎 😂":      "😎", // первый кластер
	}
	for in, want := range emojiCases {
		ref, ok := parseStickerRef(in)
		if !ok || ref.Emoji != want || ref.ID != 0 {
			t.Fatalf("%q: ref=%+v ok=%v, хочу эмодзи %q", in, ref, ok, want)
		}
	}
	for _, in := range []string{"", "null", ".", "стикер", "кот с разинутой пастью"} {
		if ref, ok := parseStickerRef(in); ok {
			t.Fatalf("%q: принято за ссылку на стикер: %+v", in, ref)
		}
	}
}

// TestStickerSchemaDescription — поле схемы объясняет модели оба способа
// выбора и прямо разрешает не заполнять поле.
func TestStickerSchemaDescription(t *testing.T) {
	props := TurnResultJSONSchema()["properties"].(map[string]any)
	sticker, ok := props["sticker"].(map[string]any)
	if !ok {
		t.Fatalf("в схеме нет поля sticker")
	}
	desc, _ := sticker["description"].(string)
	lower := strings.ToLower(desc)
	for _, want := range []string{"номер", "эмодзи", "не заполняй"} {
		if !strings.Contains(lower, want) {
			t.Fatalf("описание sticker не содержит %q: %q", want, desc)
		}
	}
}
