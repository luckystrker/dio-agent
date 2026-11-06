// Тесты этапа 38 (фиксы живого прогона 2026-11-04…06):
//
//	38.1 — схема хода и constraints строятся по ситуации, мусор необязательных
//	       строк = отсутствие поля, парсер терпит неприменимые поля;
//	38.2 — answer без текста: деградация после repair;
//	38.3 — длинные необязательные поля обрезаются, а не роняют ход;
//	38.4 — раздельные потолки инструментов по видам.
package character

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// schemaProps — свойства схемы хода по флагам ситуации.
func schemaProps(f TurnSchemaFeatures) map[string]any {
	return TurnResultJSONSchemaFor(f)["properties"].(map[string]any)
}

// TestTurnSchemaByFeatures — необязательное поле попадает в схему только когда
// применимо на этом ходе (38.1): образовывать мусор модели нечем.
func TestTurnSchemaByFeatures(t *testing.T) {
	conditional := []string{"image_url", "image_notes", "sticker", "save_sticker", "forward", "reply_to_by_message"}
	base := []string{"decision", "messages", "notes", "memory", "reaction", "intentions",
		"defer_hours", "mood_delta", "relationship_delta", "reason"}

	// Полная схема (все флаги) знает все поля — совместимость с прежними
	// тестами и вызовами.
	all := TurnSchemaFeatures{Images: true, UnknownSticker: true, StickerAllowed: true, Web: true, HistoryRefs: true}
	full := schemaProps(all)
	for _, want := range append(append([]string{}, base...), conditional...) {
		if _, ok := full[want]; !ok {
			t.Fatalf("полная схема потеряла поле %q", want)
		}
	}

	// Ни одного флага: только базовые поля.
	bare := schemaProps(TurnSchemaFeatures{})
	for _, want := range base {
		if _, ok := bare[want]; !ok {
			t.Fatalf("базовая схема потеряла поле %q", want)
		}
	}
	for _, not := range conditional {
		if _, ok := bare[not]; ok {
			t.Fatalf("неприменимое поле %q попало в схему пустого хода", not)
		}
	}

	cases := []struct {
		name string
		f    TurnSchemaFeatures
		want []string
	}{
		{"картинки пачки", TurnSchemaFeatures{Images: true}, []string{"image_notes"}},
		{"незнакомый стикер", TurnSchemaFeatures{UnknownSticker: true}, []string{"save_sticker"}},
		{"коллекция стикеров", TurnSchemaFeatures{StickerAllowed: true}, []string{"sticker"}},
		{"веб", TurnSchemaFeatures{Web: true}, []string{"image_url"}},
		{"история с метками", TurnSchemaFeatures{HistoryRefs: true}, []string{"forward", "reply_to_by_message"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			props := schemaProps(tc.f)
			for _, want := range tc.want {
				if _, ok := props[want]; !ok {
					t.Fatalf("поле %q не попало в схему применимого хода", want)
				}
			}
			// Остальные условные поля не просачиваются.
			for _, other := range conditional {
				present := false
				for _, w := range tc.want {
					if w == other {
						present = true
					}
				}
				if present {
					continue
				}
				if _, ok := props[other]; ok {
					t.Fatalf("поле %q попало в схему, хотя неприменимо", other)
				}
			}
		})
	}
}

// TestConstraintsByTurnFeatures — constraints говорят только о тех
// необязательных полях, которые есть в схеме этого хода (38.1), и всегда
// прямо предлагают decision=react для «только реакции» (38.2).
func TestConstraintsByTurnFeatures(t *testing.T) {
	bare := constraintsSectionFor(TurnSchemaFeatures{})
	for _, not := range []string{
		"- sticker —", "- save_sticker —", "- image_url —", "- image_notes —",
		"- forward —", "reply_to_by_message —",
	} {
		if strings.Contains(bare, not) {
			t.Fatalf("constraints пустого хода говорят о неприменимом поле %q:\n%s", not, bare)
		}
	}
	if !strings.Contains(bare, "хочешь только реакцию — decision=react") {
		t.Fatalf("constraints не подсказывают decision=react для реакции без текста:\n%s", bare)
	}

	full := constraintsSection()
	for _, want := range []string{
		"- sticker —", "- save_sticker —", "- image_url —", "- image_notes —",
		"- forward —", "reply_to_by_message —",
	} {
		if !strings.Contains(full, want) {
			t.Fatalf("полные constraints потеряли %q:\n%s", want, full)
		}
	}
}

// TestLivePayloadsNormalized — три живых ответа модели (ходы 234/235/238) как
// фикстуры: мусор необязательных полей = отсутствие поля, ход не теряется.
func TestLivePayloadsNormalized(t *testing.T) {
	// Ход 234: forward заполнен текстом, sticker «null», заметка «н/д».
	r, err := ParseTurnResult([]byte(`{"decision":"answer","messages":["бля, это я вчера после ночи на б"],
		"forward":"бля, это я вчера после ночи на б","sticker":"null","save_sticker":{"note":"н/д"}}`))
	if err != nil {
		t.Fatalf("ход 234 отвергнут: %v", err)
	}
	if r.Forward != "" {
		t.Fatalf("ход 234: forward с текстом не вычищен: %q", r.Forward)
	}
	if r.Sticker != "" {
		t.Fatalf("ход 234: sticker «null» не вычищен: %q", r.Sticker)
	}
	if r.SaveSticker != nil {
		t.Fatalf("ход 234: заметка «н/д» не вычищена: %+v", r.SaveSticker)
	}

	// Ход 235: answer без messages, только реакция; forward/sticker — пунктуация,
	// заметка «нечего сохранять».
	raw235 := []byte(`{"reason":"переписка","forward":"]","sticker":"]","decision":"answer",
		"reaction":"🤔","save_sticker":{"note":"нечего сохранять"}}`)
	r, err = parseTurnResult(raw235, nil)
	if err == nil || !strings.Contains(err.Error(), "answer требует") {
		t.Fatalf("ход 235: answer без messages должен уйти в repair, получено %v", err)
	}
	if r.Forward != "" || r.Sticker != "" || r.SaveSticker != nil {
		t.Fatalf("ход 235: мусор не вычищен: %+v", r)
	}
	if r.Reaction != "🤔" {
		t.Fatalf("ход 235: реакция потеряна при чистке: %q", r.Reaction)
	}

	// Ход 238: answer без messages, стикер #6, реакция, выдуманный image_url,
	// описания картинок.
	raw238 := []byte(`{"forward":".","sticker":"#6","decision":"answer","reaction":"🤔",
		"image_url":"https://static.mos.ru/photos/moscow.jpg","image_notes":["закат"],"messages":[]}`)
	r, err = parseTurnResult(raw238, nil)
	if err == nil || !strings.Contains(err.Error(), "answer требует") {
		t.Fatalf("ход 238: answer без messages должен уйти в repair, получено %v", err)
	}
	deg, changed := degradeAnswerWithoutText(r)
	if !changed {
		t.Fatal("ход 238: деградация не сработала")
	}
	if deg.Decision != DecisionReact {
		t.Fatalf("ход 238: деградация в %q, хочу react", deg.Decision)
	}
	if deg.Sticker != "#6" || deg.Reaction != "🤔" {
		t.Fatalf("ход 238: стикер/реакция потеряны при деградации: %+v", deg)
	}
	if deg.ImageURL != "" {
		t.Fatalf("ход 238: картинка осталась при react: %q", deg.ImageURL)
	}
	if err := deg.Validate(); err != nil {
		t.Fatalf("ход 238: деградированный результат не валиден: %v", err)
	}
}

// TestDegradeAnswerWithoutText — после repair пустой answer: есть реакция или
// стикер — react (стикер остаётся), иначе ignore (38.2).
func TestDegradeAnswerWithoutText(t *testing.T) {
	cases := []struct {
		name string
		in   TurnResult
		want string
	}{
		{"только реакция", TurnResult{Decision: DecisionAnswer, Reaction: "🤔"}, DecisionReact},
		{"стикер и реакция", TurnResult{Decision: DecisionAnswer, Sticker: "#6", Reaction: "🤔"}, DecisionReact},
		{"только стикер", TurnResult{Decision: DecisionAnswer, Sticker: "#6"}, DecisionReact},
		{"ничего", TurnResult{Decision: DecisionAnswer, Forward: "#m1"}, DecisionIgnore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deg, changed := degradeAnswerWithoutText(tc.in)
			if !changed {
				t.Fatal("деградация не сработала")
			}
			if deg.Decision != tc.want {
				t.Fatalf("decision = %q, хочу %q", deg.Decision, tc.want)
			}
			if deg.Sticker != tc.in.Sticker {
				t.Fatalf("стикер потерян: %q", deg.Sticker)
			}
			if tc.want == DecisionIgnore && (deg.Forward != "" || deg.ImageURL != "") {
				t.Fatalf("при ignore остались действия: %+v", deg)
			}
			if err := deg.Validate(); err != nil {
				t.Fatalf("деградированный результат не валиден: %v", err)
			}
		})
	}
}

// TestOptionalJunkNormalized — общая нормализация мусора необязательных строк
// (38.1): пусто/null/none/nil/н/д/нет/-/пунктуация/«нечего…»/«не используется…»
// = поле не задано.
func TestOptionalJunkNormalized(t *testing.T) {
	junks := []string{"", "null", "None", "nil", "н/д", "нет", "н", "-", ".", "]", "…",
		"нечего сохранять", "не используется в этом ходу"}
	for _, junk := range junks {
		note, _ := json.Marshal(junk)

		// Заметки наблюдения: мусорная строка не проходит дальше.
		raw := fmt.Sprintf(`{"decision":"ignore","notes":[%s,"дельная заметка"]}`, note)
		r, err := ParseTurnResult([]byte(raw))
		if err != nil {
			t.Fatalf("мусорная заметка %q отвергла ход: %v", junk, err)
		}
		if len(r.Notes) != 1 || r.Notes[0] != "дельная заметка" {
			t.Fatalf("мусорная заметка %q не вычищена: %+v", junk, r.Notes)
		}

		// Верхнеуровневый reason.
		raw = fmt.Sprintf(`{"decision":"ignore","reason":%s}`, note)
		r, err = ParseTurnResult([]byte(raw))
		if err != nil {
			t.Fatalf("мусорный reason %q отверг ход: %v", junk, err)
		}
		if r.Reason != "" {
			t.Fatalf("мусорный reason %q не вычищен: %q", junk, r.Reason)
		}

		// Основание дельты настроения: мусор = дельты нет.
		raw = fmt.Sprintf(`{"decision":"ignore","mood_delta":{"valence":0.1,"reason":%s}}`, note)
		r, err = ParseTurnResult([]byte(raw))
		if err != nil {
			t.Fatalf("мусорное основание %q отвергло ход: %v", junk, err)
		}
		if r.MoodDelta != nil {
			t.Fatalf("дельта с мусорным основанием %q осталась: %+v", junk, r.MoodDelta)
		}

		// Метка пересылки: не метка — пересылки нет.
		raw = fmt.Sprintf(`{"decision":"answer","messages":["ок"],"forward":%s}`, note)
		r, err = ParseTurnResult([]byte(raw))
		if err != nil {
			t.Fatalf("мусорная пересылка %q отвергла ход: %v", junk, err)
		}
		if r.Forward != "" {
			t.Fatalf("мусорная пересылка %q не вычищена", junk)
		}
	}
}

// TestOptionalLongFieldsTruncated — длинные необязательные строки режутся по
// рунам с «…», а не роняют ход (38.3).
func TestOptionalLongFieldsTruncated(t *testing.T) {
	long := strings.Repeat("я", 600)
	note, _ := json.Marshal(long)
	veryLong := strings.Repeat("я", maxNoteLength+50)
	notes, _ := json.Marshal(veryLong)

	cases := []struct {
		name string
		raw  string
		get  func(TurnResult) string
		max  int
	}{
		{
			"заметка сохранения стикера",
			fmt.Sprintf(`{"decision":"answer","messages":["ок"],"save_sticker":{"note":%s}}`, note),
			func(r TurnResult) string { return r.SaveSticker.Note }, stickerNoteLimit,
		},
		{
			"описание картинки",
			fmt.Sprintf(`{"decision":"answer","messages":["ок"],"image_notes":[%s]}`, note),
			func(r TurnResult) string { return r.ImageNotes[0] }, maxImageNoteLength,
		},
		{
			"основание настроения",
			fmt.Sprintf(`{"decision":"ignore","mood_delta":{"valence":0.1,"reason":%s}}`, note),
			func(r TurnResult) string { return r.MoodDelta.Reason }, maxReasonLength,
		},
		{
			"основание отношений",
			fmt.Sprintf(`{"decision":"ignore","relationship_delta":{"trust":0.1,"reason":%s}}`, note),
			func(r TurnResult) string { return r.RelationshipDelta.Reason }, maxReasonLength,
		},
		{
			"тема намерения",
			fmt.Sprintf(`{"decision":"answer","messages":["ок"],"intentions":[{"kind":"check_in","topic":%s,"reason":"просто так","delay_hours":3}]}`, note),
			func(r TurnResult) string { return r.Intentions[0].Topic }, maxIntentionTopicLength,
		},
		{
			"основание намерения",
			fmt.Sprintf(`{"decision":"answer","messages":["ок"],"intentions":[{"kind":"check_in","topic":"как дела","reason":%s,"delay_hours":3}]}`, note),
			func(r TurnResult) string { return r.Intentions[0].Reason }, maxReasonLength,
		},
		{
			"заметка наблюдения",
			fmt.Sprintf(`{"decision":"ignore","notes":[%s]}`, notes),
			func(r TurnResult) string { return r.Notes[0] }, maxNoteLength,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ParseTurnResult([]byte(tc.raw))
			if err != nil {
				t.Fatalf("длинное поле отвергло ход: %v", err)
			}
			got := tc.get(r)
			if n := utf8.RuneCountInString(got); n != tc.max {
				t.Fatalf("длина = %d, хочу %d", n, tc.max)
			}
			if !strings.HasSuffix(got, "…") {
				t.Fatalf("обрезка без «…»: %q", got[len(got)-8:])
			}
		})
	}
}

// TestStructuralErrorsRemainTerminal — обязательные структурные ошибки по-
// прежнему ошибка (38.3), нормализация их не глотает.
func TestStructuralErrorsRemainTerminal(t *testing.T) {
	cases := map[string]string{
		"пустое сообщение":        `{"decision":"answer","messages":[""]}`,
		"пустой текст памяти":     `{"decision":"ignore","memory":[{"kind":"claim","text":""}]}`,
		"defer без defer_hours":   `{"decision":"defer"}`,
		"react без реакции":       `{"decision":"react"}`,
		"неизвестный decision":    `{"decision":"shout"}`,
		"слишком много сообщений": `{"decision":"answer","messages":["1","2","3","4","5"]}`,
	}
	for name, raw := range cases {
		if _, err := ParseTurnResult([]byte(raw)); err == nil {
			t.Fatalf("%s: ошибка не поймана", name)
		}
	}
}

// TestToolLimitPerKindCaps — 38.4: у вида инструментов свой потолок; память на
// ходе с вебом не может съесть общий лимит.
func TestToolLimitPerKindCaps(t *testing.T) {
	m := NewToolLimitMiddleware(5, nil)
	m.SetKindCaps(conversationKindCaps(true, 2, true, "web_search"))

	executed := 0
	next := stubToolExec(&executed)
	call := func(name string) events.Event {
		t.Helper()
		out, err := m.OnToolExecution(next, &events.ToolCallEvent{ID: name, Name: name}, nil)
		if err != nil {
			t.Fatalf("вызов %s: %v", name, err)
		}
		return out
	}

	// Память: первые два вызова исполняются, третий — отказ с текстом про память.
	for i := 1; i <= 2; i++ {
		if ev := call("memory_search"); ev == nil {
			t.Fatalf("вызов памяти %d потерян", i)
		}
	}
	ev := call("memory_find_entities")
	te, ok := ev.(*events.ToolErrorEvent)
	if !ok {
		t.Fatalf("третье обращение к памяти не отклонено: %T", ev)
	}
	if !strings.Contains(te.Error, "памяти") || !strings.Contains(te.Error, "отвечай") {
		t.Fatalf("модели нужен внятный отказ памяти, получено %q", te.Error)
	}
	if executed != 2 {
		t.Fatalf("исполнено вызовов %d, хочу 2", executed)
	}

	// Веб живёт своим потолком: память исчерпана, поиск ещё можно.
	for i := 1; i <= 2; i++ {
		call("web_search")
	}
	ev = call("web_search")
	if te, ok := ev.(*events.ToolErrorEvent); !ok || !strings.Contains(te.Error, "поиск") {
		t.Fatalf("третий поиск не отклонён своим лимитом: %#v", ev)
	}
	if executed != 4 {
		t.Fatalf("исполнено вызовов %d, хочу 4 (2 памяти + 2 поиска)", executed)
	}
}

// TestConversationKindCaps — потолки видов: память ≤2, поиск ≤2, страницы ≤2;
// конфигурация может только снизить (38.4).
func TestConversationKindCaps(t *testing.T) {
	caps := conversationKindCaps(true, 2, true, "web_search")
	want := map[string]int{"memory": 2, "search": 2, "fetch": 2}
	for _, cap := range caps {
		if cap.Limit != want[cap.Kind] {
			t.Fatalf("потолок %s = %d, хочу %d", cap.Kind, cap.Limit, want[cap.Kind])
		}
	}
	if len(caps) != len(want) {
		t.Fatalf("видов %d, хочу %d: %+v", len(caps), len(want), caps)
	}

	// Конфигурация ниже — уважаем; выше/ноль — остаётся 2.
	low := conversationKindCaps(true, 1, true, "web_search")
	for _, cap := range low {
		if cap.Kind == "memory" && cap.Limit != 1 {
			t.Fatalf("память = %d, хочу 1 (конфиг ниже)", cap.Limit)
		}
	}
	// Без веба веб-видов нет, память не больше 2.
	noWeb := conversationKindCaps(true, 0, false, "")
	for _, cap := range noWeb {
		if cap.Kind != "memory" || cap.Limit != memoryToolsLimit {
			t.Fatalf("без веба ожидался только потолок памяти 2: %+v", cap)
		}
	}
}

// TestToolLimitKindCapsKeepsOtherKinds — отказ вида памяти не отключает
// инструменты целиком: веб продолжает работать (38.4).
func TestToolLimitKindCapsKeepsOtherKinds(t *testing.T) {
	m := NewToolLimitMiddleware(5, nil)
	disabled := false
	m.SetOnExhausted(func() { disabled = true })
	m.SetKindCaps(conversationKindCaps(true, 2, true, "web_search"))

	executed := 0
	next := stubToolExec(&executed)
	cx := agocontext.New(nil)
	for i := 0; i < 3; i++ {
		if _, err := m.OnToolExecution(next, &events.ToolCallEvent{ID: "m", Name: "memory_search"}, cx); err != nil {
			t.Fatalf("вызов памяти: %v", err)
		}
	}
	if disabled {
		t.Fatal("исчерпание вида памяти отключило все инструменты — веб должен остаться")
	}
	if _, err := m.OnToolExecution(next, &events.ToolCallEvent{ID: "w", Name: "web_search"}, cx); err != nil {
		t.Fatalf("поиск после отказа памяти: %v", err)
	}
	if executed != 3 {
		t.Fatalf("исполнено %d, хочу 3 (2 памяти + 1 поиск)", executed)
	}
}
