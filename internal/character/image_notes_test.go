// Тесты описаний картинок (этап 37.1): нумерация картинок пачки, перенос
// image_notes в события и рендер истории с описанием.
package character

import (
	"strings"
	"testing"
)

func TestParseTurnResultImageNotes(t *testing.T) {
	ok, err := ParseTurnResult([]byte(`{"decision":"answer","messages":["красиво"],"image_notes":["закат над морем","кот в коробке"]}`))
	if err != nil {
		t.Fatalf("валидные image_notes отклонены: %v", err)
	}
	if len(ok.ImageNotes) != 2 || ok.ImageNotes[0] != "закат над морем" || ok.ImageNotes[1] != "кот в коробке" {
		t.Fatalf("image_notes = %v", ok.ImageNotes)
	}

	// Каждая фраза короткая; пустые позиции допустимы (модель пропускает
	// неинтересную картинку, но порядок позиций не сдвигается).
	ok, err = ParseTurnResult([]byte(`{"decision":"ignore","image_notes":["  ","пёс у окна"]}`))
	if err != nil {
		t.Fatalf("пустая позиция описания отклонена: %v", err)
	}
	if len(ok.ImageNotes) != 2 || ok.ImageNotes[0] != "" || ok.ImageNotes[1] != "пёс у окна" {
		t.Fatalf("позиции описаний сдвинуты: %v", ok.ImageNotes)
	}

	// Длинное описание режется по рунам с «…» (38.3), а не роняет ход: это
	// необязательная фраза, её потеря не отменяет ответ.
	long, err := ParseTurnResult([]byte(`{"decision":"ignore","image_notes":["` + strings.Repeat("а", maxImageNoteLength+1) + `"]}`))
	if err != nil {
		t.Fatalf("длинное описание отвергло ход: %v", err)
	}
	if n := len([]rune(long.ImageNotes[0])); n != maxImageNoteLength {
		t.Fatalf("описание обрезано до %d рун, хочу %d", n, maxImageNoteLength)
	}
	raw := `{"decision":"ignore","image_notes":["`
	parts := make([]string, maxImageNotes+1)
	for i := range parts {
		parts[i] = "х"
	}
	raw += strings.Join(parts, `","`) + `"]}`
	if _, err := ParseTurnResult([]byte(raw)); err == nil {
		t.Fatal("описаний больше картинок хода принято")
	}
}

func TestImageNumberingLine(t *testing.T) {
	groups := []ImageGroup{
		{EventID: 1, Kind: imageKindPhoto, First: 1, Count: 1, Label: "фото от Данила"},
		{EventID: 2, Kind: imageKindGIF, First: 2, Count: 5, Label: "кадры гифки"},
	}
	line := ImageNumberingLine(groups)
	// 41.2 M7: нумерация — позиции групп, кадры гифки — одна позиция.
	for _, want := range []string{"1 — фото от Данила", "2 — кадры гифки (5 кадров)", "всего 2"} {
		if !strings.Contains(line, want) {
			t.Fatalf("нумерация %q не содержит %q", line, want)
		}
	}
	if line := ImageNumberingLine(nil); line != "" {
		t.Fatalf("без картинок нумерация не нужна, получено %q", line)
	}
}

// TestImageNotesForEvents — одно описание на видео (кадры одного видео — один
// слот), лишние описания отбрасываются, пустые позиции не затирают соседей.
func TestImageNotesForEvents(t *testing.T) {
	groups := []ImageGroup{
		{EventID: 11, Kind: imageKindPhoto, First: 1, Count: 1},
		{EventID: 12, Kind: imageKindSticker, First: 2, Count: 1},
		{EventID: 13, Kind: imageKindGIF, First: 3, Count: 5},
	}
	notes := []string{"закат", "кот", "прыжок", "лишнее"}
	got := ImageNotesForEvents(groups, notes)
	if len(got) != 3 {
		t.Fatalf("описаний сопоставлено %d, хочу 3: %+v", len(got), got)
	}
	if got[0].EventID != 11 || got[0].Note != "закат" || got[0].Kind != imageKindPhoto {
		t.Fatalf("фото: %+v", got[0])
	}
	if got[2].EventID != 13 || got[2].Note != "прыжок" || got[2].Kind != imageKindGIF {
		t.Fatalf("гифка: %+v", got[2])
	}
	if got := ImageNotesForEvents(groups, []string{"", "кот"}); len(got) != 1 || got[0].EventID != 12 {
		t.Fatalf("пустая позиция не должна сдвигать сопоставление: %+v", got)
	}
}

func TestRenderIncomingContentWithNote(t *testing.T) {
	cases := []struct {
		name, kind, text, noteKind, note, want string
	}{
		{"фото без подписи", "photo", "", imageKindPhoto, "закат над морем", "[фото: закат над морем]"},
		{"фото с подписью", "photo", "смотри", imageKindPhoto, "закат над морем", "[фото: закат над морем] смотри"},
		{"стикер", "sticker", "[стикер 😂]", imageKindSticker, "кот смеётся", "[стикер 😂: кот смеётся]"},
		{"стикер без эмодзи", "sticker", "[стикер]", imageKindSticker, "кот смеётся", "[стикер: кот смеётся]"},
		{"гифка", "message", "[гифка] весёлый прыжок", imageKindGIF, "пёс прыгает", "[гифка: пёс прыгает] весёлый прыжок"},
		{"кружок", "message", "[кружок, 0:15]", imageKindVideoNote, "человек машет", "[кружок 0:15: человек машет]"},
		{"реплай с фото", "reply", "[в ответ на Коля: «…»] ", imageKindPhoto, "закат", "[фото: закат] [в ответ на Коля: «…»] "},
		{"нет описания — как раньше", "photo", "", imageKindPhoto, "", "[фото]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderIncomingContent(tc.kind, tc.text, tc.noteKind, tc.note); got != tc.want {
				t.Fatalf("renderIncomingContent = %q, хочу %q", got, tc.want)
			}
		})
	}
}

func TestRenderAssistantPhoto(t *testing.T) {
	cases := []struct {
		name, payload, want string
	}{
		{"alt со страницы", `{"kind":"photo","source_url":"https://cdn.example.com/a.jpg","alt":"Кот в коробке"}`, "[отправил картинку: Кот в коробке]"},
		{"без alt — домен", `{"kind":"photo","source_url":"https://cdn.example.com/a.jpg"}`, "[отправил картинку: cdn.example.com]"},
		{"без адреса", `{"kind":"photo"}`, "[отправил картинку]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderAssistantPhoto([]byte(tc.payload)); got != tc.want {
				t.Fatalf("renderAssistantPhoto = %q, хочу %q", got, tc.want)
			}
		})
	}
}
