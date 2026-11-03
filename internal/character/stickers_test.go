// Юнит-тесты коллекции стикеров (этап 25.3–25.4): метка стикера в панели и в
// TurnResult, потолки списка, разбор секции persona-файла.
package character

import (
	"strings"
	"testing"
)

// TestParseStickerLabel — метка коллекции: «#3» и «3» равнозначны (модель
// часто опускает решётку), мусор не становится стикером. С 28.2 разбор
// терпимый к живым формам метки: «,#1», « #1 », «#1.», «№1» — та же метка
// (модель добавляет запятую и точку как часть перечисления, пишет №).
func TestParseStickerLabel(t *testing.T) {
	for _, in := range []string{"#3", "3", " #12 ", "#07"} {
		id, ok := ParseStickerLabel(in)
		if !ok || id == 0 {
			t.Fatalf("метка %q не разобрана: id=%d ok=%v", in, id, ok)
		}
	}
	tolerant := map[string]int64{
		",#1":   1, // ход 123: запятая перед меткой не распознана
		" #1 ":  1,
		"#1.":   1,
		"1":     1,
		"№1":    1,
		"#2,":   2,
		"№ 12.": 12,
	}
	for in, want := range tolerant {
		id, ok := ParseStickerLabel(in)
		if !ok || id != want {
			t.Fatalf("метка %q не разобрана терпимо: id=%d ok=%v, хочу %d", in, id, ok, want)
		}
	}
	for _, in := range []string{"", "#", ".,", "😂", "#-1", "#3a", "три", "стикер 3", "1 2", "999999999999999999999999"} {
		if id, ok := ParseStickerLabel(in); ok {
			t.Fatalf("мусор %q принят за метку: id=%d", in, id)
		}
	}
}

// TestRenderStickerList — панель: метка, эмодзи и заметка; пустая коллекция —
// строки нет; объём ограничен, чужие квадратные скобки не ломают панель.
func TestRenderStickerList(t *testing.T) {
	if got := RenderStickerList(nil); got != "" {
		t.Fatalf("пустая коллекция — пустая строка, получено %q", got)
	}
	out := RenderStickerList([]Sticker{
		{ID: 3, Emoji: "😂", Note: "ржу над тупой шуткой [не панель]"},
		{ID: 7, Emoji: "🖕", Note: "послать нахуй"},
	})
	for _, want := range []string{"#3", "😂", "ржу над тупой шуткой", "#7", "🖕", "послать"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в списке нет %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[не панель]") {
		t.Fatalf("квадратные скобки заметки не санитизированы:\n%s", out)
	}
	if !strings.Contains(out, "sticker") {
		t.Fatalf("в списке нет подсказки про поле sticker:\n%s", out)
	}

	// Потолок: много стикеров — список короче и в пределах объёма.
	var many []Sticker
	for i := 1; i <= 50; i++ {
		many = append(many, Sticker{ID: int64(i), Emoji: "🔥", Note: strings.Repeat("когда уместен ", 20)})
	}
	got := RenderStickerList(many)
	if n := strings.Count(got, "\n"); n > stickerPanelItems {
		t.Fatalf("строк списка %d, потолок %d", n, stickerPanelItems)
	}
	if runes := []rune(got); len(runes) > stickerPanelMaxChars+stickerPanelHeaderOverhead {
		t.Fatalf("объём списка %d рун сверх потолка %d", len(runes), stickerPanelMaxChars)
	}
}

// TestParsePersonaStickers — секция «Стикеры» persona-файла: строки
// «file_id | эмодзи | заметка», пустая секция допустима (владелец заполнит).
func TestParsePersonaStickers(t *testing.T) {
	base := `## Имя
Дио
## Описание
живой
## Манера речи
коротко
## Ценности
- прямота
## Настроение (базовое)
valence: 0.1
arousal: 0.5
socialEnergy: 0.9
`
	empty, err := ParsePersonaFile(base + "## Стикеры\n")
	if err != nil {
		t.Fatalf("пустая секция стикеров — не ошибка: %v", err)
	}
	if len(empty.Stickers) != 0 {
		t.Fatalf("пустая секция дала стикеры: %+v", empty.Stickers)
	}

	withList, err := ParsePersonaFile(base + `## Стикеры
CAACAgIAAxkBAAIB | 😂 | ржу над тупой шуткой
CAACAgIAAxkBAAIC | 🖕 | послать
`)
	if err != nil {
		t.Fatalf("секция стикеров не разобрана: %v", err)
	}
	if len(withList.Stickers) != 2 {
		t.Fatalf("стикеров %d, хочу 2: %+v", len(withList.Stickers), withList.Stickers)
	}
	if s := withList.Stickers[0]; s.FileID != "CAACAgIAAxkBAAIB" || s.Emoji != "😂" || s.Note != "ржу над тупой шуткой" {
		t.Fatalf("первый стикер разобран неверно: %+v", s)
	}

	// Битая строка — ошибка с понятным форматом, а не тихий пропуск.
	if _, err := ParsePersonaFile(base + "## Стикеры\nпросто файл без разделителей\n"); err == nil {
		t.Fatal("строка без «file_id | эмодзи | заметка» должна валить запуск")
	}
	if _, err := ParsePersonaFile(base + "## Стикеры\n | 😂 | без file_id\n"); err == nil {
		t.Fatal("пустой file_id должен валить запуск")
	}
}
