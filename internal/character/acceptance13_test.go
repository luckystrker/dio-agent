// Приёмочные тесты этапа 13 (фиксы ручной приёмки Р): ожидания — из спеки и
// пунктов CHECKLIST 13.1/13.7, не из устройства кода. Отдельный файл, чтобы не
// смешивать с юнит-тестами модулей.
package character

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

// Контракт 13.7: PersonaSource.Interests — список строк.
var acc13InterestsIsStringSlice []string = PersonaSource{}.Interests

// acc13PersonaText — валидный persona-файл с обязательными секциями и
// необязательным хвостом «Интересы» (какой передали — такой и вклеен).
func acc13PersonaText(interestsSection string) string {
	return `## Имя
Дио

## Описание
Живой собеседник в телеграме.

## Манера речи
Строчные, коротко, без тире.

## Ценности
- честность
- интерес к людям

## Настроение (базовое)
valence: 0.2
arousal: 0.4
socialEnergy: 0.3
` + interestsSection
}

// --- 13.7 Секция «Интересы» ---

// Секция «Интересы» читается списком с дефисов — паритет «Ценностей».
func TestAcc13PersonaInterestsParsedLikeValues(t *testing.T) {
	text := acc13PersonaText(`
## Интересы
- космос
- история вина
-   лыжи с гор
`)
	src, err := ParsePersonaFile(text)
	if err != nil {
		t.Fatalf("валидный persona-файл не разобран: %v", err)
	}
	want := []string{"космос", "история вина", "лыжи с гор"}
	if len(src.Interests) != len(want) {
		t.Fatalf("интересов %d (%v), ожидалось %v", len(src.Interests), src.Interests, want)
	}
	for i, w := range want {
		if src.Interests[i] != w {
			t.Errorf("интерес[%d] = %q, ожидался %q", i, src.Interests[i], w)
		}
	}
	// паритет «Ценностей» не пострадал
	if len(src.Values) != 2 || src.Values[0] != "честность" {
		t.Errorf("ценности = %v, ожидались [честность интерес к людям]", src.Values)
	}
}

// Секция есть, но пуста — ошибка, называющая секцию.
func TestAcc13PersonaInterestsEmptySectionIsError(t *testing.T) {
	cases := map[string]string{
		"вообще пустая":    "\n## Интересы\n",
		"только пробелы":   "\n## Интересы\n   \n",
		"дефис без текста": "\n## Интересы\n- \n",
		"пустые строки":    "\n## Интересы\n\n\n",
	}
	for name, section := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePersonaFile(acc13PersonaText(section))
			if err == nil {
				t.Fatal("ожидалась ошибка пустой секции, файл разобран")
			}
			if !strings.Contains(err.Error(), "Интересы") {
				t.Errorf("ошибка не называет секцию: %q", err.Error())
			}
		})
	}
}

// Секции нет — Interests nil (не пустой список, а именно nil).
func TestAcc13PersonaNoInterestsSectionGivesNil(t *testing.T) {
	src, err := ParsePersonaFile(acc13PersonaText(""))
	if err != nil {
		t.Fatalf("валидный persona-файл не разобран: %v", err)
	}
	if src.Interests != nil {
		t.Errorf("Interests = %v, ожидался nil", src.Interests)
	}
}

// Неизвестная секция по-прежнему ошибка — новая известная секция не открыла дверь.
func TestAcc13PersonaUnknownSectionStillRejected(t *testing.T) {
	text := acc13PersonaText("") + "\n## Любимые книги\n- тут были книги\n"
	_, err := ParsePersonaFile(text)
	if err == nil {
		t.Fatal("ожидалась ошибка неизвестной секции")
	}
	if !strings.Contains(err.Error(), "Любимые книги") {
		t.Errorf("ошибка не называет неизвестную секцию: %q", err.Error())
	}
}

// Рендер персоны: при непустых интересах есть строка «Темы, о которых тебе
// интересно говорить: …», при пустых — строки нет.
func TestAcc13RenderPersonaBlockInterests(t *testing.T) {
	base := Persona{
		Name:        "Дио",
		Description: "Живой собеседник",
		SpeechStyle: "коротко",
		Values:      []string{"честность"},
	}
	withInterests := base
	withInterests.Interests = []string{"космос", "вино"}
	block := RenderPersonaBlock(withInterests)
	if !strings.Contains(block, "Темы, о которых тебе интересно говорить:") {
		t.Errorf("в блоке персоны нет строки интересов:\n%s", block)
	}
	if !strings.Contains(block, "космос") || !strings.Contains(block, "вино") {
		t.Errorf("строка интересов без самих интересов:\n%s", block)
	}

	for name, interests := range map[string][]string{"nil": nil, "пустой список": {}} {
		p := base
		p.Interests = interests
		block := RenderPersonaBlock(p)
		if strings.Contains(block, "интересно говорить") {
			t.Errorf("пустые интересы (%s) оставили строку интересов:\n%s", name, block)
		}
	}
}

// --- 13.1 Vision-стык: части модельного запроса [текст, image...] ---

// Юнит-шов здесь — сборка аргументов Ask: первый элемент — текст пачки,
// дальше по одному image-входу на фото, с исходными байтами и mime.
// Сам провод Runner.generate → agent.Ask юнитом без БД не закрывается
// (он проверен e2e-тестом vision из чек-листа 13.1).
func TestAcc13AskMsgsTextThenImageParts(t *testing.T) {
	jpeg := []byte{1, 2, 3, 4}
	png := []byte{9, 8}
	msgs := askMsgs("смотри, что прислали", []Photo{
		{FileID: "f1", Data: jpeg, Mime: "image/jpeg"},
		{FileID: "f2", Data: png, Mime: "image/png"},
	})

	if len(msgs) != 3 {
		t.Fatalf("частей %d, ожидалось 3 (текст + 2 фото)", len(msgs))
	}
	text, ok := msgs[0].(string)
	if !ok || text != "смотри, что прислали" {
		t.Fatalf("первая часть = %#v, ожидался текст пачки", msgs[0])
	}

	wantData := [][]byte{jpeg, png}
	wantMime := []string{"image/jpeg", "image/png"}
	for i, part := range msgs[1:] {
		bin, ok := part.(*events.BinaryInput)
		if !ok {
			t.Fatalf("часть %d не image-вход: %#v", i+1, part)
		}
		if bin.Kind != events.BinaryImage {
			t.Errorf("часть %d: kind = %q, ожидался image", i+1, bin.Kind)
		}
		if bin.MediaType != wantMime[i] {
			t.Errorf("часть %d: media type = %q, ожидался %q", i+1, bin.MediaType, wantMime[i])
		}
		if !bytes.Equal(bin.Data, wantData[i]) {
			t.Errorf("часть %d: байты фото исказились (%d байт, ожидалось %d)", i+1, len(bin.Data), len(wantData[i]))
		}
	}
}

// Фото нет — аргумент Ask состоит из одного текста.
func TestAcc13AskMsgsTextOnlyWithoutPhotos(t *testing.T) {
	msgs := askMsgs("просто текст", nil)
	if len(msgs) != 1 {
		t.Fatalf("частей %d, ожидалась 1", len(msgs))
	}
	if text, ok := msgs[0].(string); !ok || text != "просто текст" {
		t.Fatalf("единственная часть = %#v, ожидался текст", msgs[0])
	}
}

// --- 13.2 Цитата реплая доезжает в ход ---

// Юнит-шов «цитата доезжает в ход»: единственный носитель цитаты — текст
// события (NormalizeUpdate), и он без потерь становится текстом пачки
// (batchTexts → пользовательский текст запуска агента). Хоп social_events →
// BatchEvent идёт через БД и остаётся на e2e-приёмке.
func TestAcc13ReplyQuoteFlowsIntoTurnUserText(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"update_id": 9,
		"message": map[string]any{
			"message_id": 11,
			"from":       map[string]any{"id": 7, "first_name": "Оля"},
			"chat":       map[string]any{"id": 42, "type": "private"},
			"text":       "а теперь?",
			"reply_to_message": map[string]any{
				"message_id": 5,
				"from":       map[string]any{"id": 3, "first_name": "Вася"},
				"text":       "старое железо ценю",
			},
		},
	})
	if err != nil {
		t.Fatalf("тестовый update не сериализуется: %v", err)
	}
	ev, err := telegram.NormalizeUpdate(raw)
	if err != nil {
		t.Fatalf("нормализация упала: %v", err)
	}
	if ev.Text == nil {
		t.Fatal("текст события пуст — цитате некуда доезжать")
	}

	batch := []BatchEvent{{EventID: 1, ActorID: 7, ActorName: "Оля", Text: *ev.Text}}
	userText := strings.Join(batchTexts(batch), "\n")
	if !strings.Contains(userText, "в ответ на Вася") || !strings.Contains(userText, "старое железо ценю") {
		t.Errorf("цитата реплая потерялась по дороге в текст хода: %q", userText)
	}
	if !strings.Contains(userText, "а теперь?") {
		t.Errorf("собственный текст события потерялся: %q", userText)
	}
}
