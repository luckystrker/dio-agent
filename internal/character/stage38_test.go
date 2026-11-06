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

// TestTurnSchemaStableAcrossFeatures — с 39.2 JSON-схема хода ОДНА и та же на
// всех разговорных ходах (полная), независимо от ситуации: иначе префикс
// запроса менялся и кэш провайдера сбрасывался. Флаги ситуации теперь влияют
// только на подсказки панели и мягкое обнуление в runtime.
func TestTurnSchemaStableAcrossFeatures(t *testing.T) {
	combos := []TurnSchemaFeatures{
		{},
		{Images: true},
		{UnknownSticker: true},
		{StickerAllowed: true},
		{Web: true},
		{HistoryRefs: true},
		allTurnSchemaFeatures(),
	}
	base, err := json.Marshal(TurnResultJSONSchemaFor(combos[0]))
	if err != nil {
		t.Fatalf("схема не сериализуется: %v", err)
	}
	for i, f := range combos[1:] {
		got, err := json.Marshal(TurnResultJSONSchemaFor(f))
		if err != nil {
			t.Fatalf("схема %d не сериализуется: %v", i+1, err)
		}
		if string(got) != string(base) {
			t.Fatalf("схема хода %d отличается от базовой — префикс запроса нестабилен", i+1)
		}
	}
	// Полная схема знает все поля TurnResult.
	props := TurnResultJSONSchemaFor(TurnSchemaFeatures{})["properties"].(map[string]any)
	for _, want := range []string{"decision", "messages", "notes", "memory", "reaction", "intentions",
		"defer_hours", "mood_delta", "relationship_delta", "reason", "image_url", "image_notes",
		"sticker", "save_sticker", "forward", "reply_to_by_message"} {
		if _, ok := props[want]; !ok {
			t.Fatalf("стабильная схема потеряла поле %q", want)
		}
	}
}

// TestConstraintsStableAcrossFeatures — constraints идут в system-промпт,
// поэтому с 39.2 их текст одинаков на всех ходах: ситуативные подсказки
// переехали в панель (turnFieldHints), и префикс запроса не меняется.
func TestConstraintsStableAcrossFeatures(t *testing.T) {
	base := constraintsSectionFor(TurnSchemaFeatures{})
	combos := []TurnSchemaFeatures{
		{Images: true}, {UnknownSticker: true}, {StickerAllowed: true},
		{Web: true}, {HistoryRefs: true}, allTurnSchemaFeatures(),
	}
	for i, f := range combos {
		if got := constraintsSectionFor(f); got != base {
			t.Fatalf("constraints хода %d отличаются от базовых — префикс запроса нестабилен", i+1)
		}
	}
	for _, want := range []string{
		"хочешь только реакцию — decision=react", "- sticker —", "- save_sticker —",
		"- image_url —", "- image_notes —", "- forward —", "reply_to_by_message —",
	} {
		if !strings.Contains(base, want) {
			t.Fatalf("стабильные constraints потеряли %q:\n%s", want, base)
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

	// Память: первые два вызова исполняются, третий — отказ с текстом про
	// память и закрытием инструментов (39.1).
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
	if !strings.Contains(te.Error, "памяти") || !strings.Contains(te.Error, "закрыты") {
		t.Fatalf("модели нужен внятный отказ памяти и закрытие, получено %q", te.Error)
	}
	if executed != 2 {
		t.Fatalf("исполнено вызовов %d, хочу 2", executed)
	}
	// После закрытия любой другой вид тоже закрыт — вызов на СЛЕДУЮЩЕМ
	// модельном ходу замыкает ход краном (B1: в том же блоке крана нет).
	modelTurn(t, m)
	ev = call("web_search")
	if tr, ok := ev.(*events.ToolResultEvent); !ok || !tr.Result.Final {
		t.Fatalf("после закрытия веб-вызов не замкнут стоп-краном: %T", ev)
	}

	// Веб-потолки проверяем на свежем middleware (закрытие — на весь ход).
	web := NewToolLimitMiddleware(5, nil)
	web.SetKindCaps(conversationKindCaps(true, 2, true, "web_search"))
	executed = 0
	next = stubToolExec(&executed)
	callWeb := func(name string) events.Event {
		t.Helper()
		out, err := web.OnToolExecution(next, &events.ToolCallEvent{ID: name, Name: name}, nil)
		if err != nil {
			t.Fatalf("вызов %s: %v", name, err)
		}
		return out
	}
	for i := 1; i <= 2; i++ {
		callWeb("web_search")
	}
	ev = callWeb("web_search")
	if te, ok := ev.(*events.ToolErrorEvent); !ok || !strings.Contains(te.Error, "поиск") {
		t.Fatalf("третий поиск не отклонён своим лимитом: %#v", ev)
	}
	if executed != 2 {
		t.Fatalf("исполнено поисков %d, хочу 2", executed)
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

// TestToolLimitKindRejectClosesTools — с 39.1 первый отказ вида (38.4) — это
// первый отказ хода: инструменты закрываются (tool_choice=none), отказ несёт
// подсказку «ответь текстом», а повторный вызов при уже закрытых инструментах
// замыкает ход стоп-краном (модель игнорирует tool_choice=none).
func TestToolLimitKindRejectClosesTools(t *testing.T) {
	m := NewToolLimitMiddleware(5, nil)
	closed := 0
	m.SetOnExhausted(func() { closed++ })
	m.SetKindCaps(conversationKindCaps(true, 2, true, "web_search"))

	executed := 0
	next := stubToolExec(&executed)
	cx := agocontext.New(nil)
	for i := 0; i < 2; i++ {
		if _, err := m.OnToolExecution(next, &events.ToolCallEvent{ID: "m", Name: "memory_search"}, cx); err != nil {
			t.Fatalf("вызов памяти: %v", err)
		}
	}
	ev, err := m.OnToolExecution(next, &events.ToolCallEvent{ID: "m3", Name: "memory_search"}, cx)
	if err != nil {
		t.Fatalf("третий вызов памяти: %v", err)
	}
	te, ok := ev.(*events.ToolErrorEvent)
	if !ok {
		t.Fatalf("третье обращение к памяти не отклонено: %T", ev)
	}
	if !strings.Contains(te.Error, "закрыты") || !strings.Contains(te.Error, "текстом") {
		t.Fatalf("отказ не говорит, что инструменты закрыты: %q", te.Error)
	}
	if closed != 1 {
		t.Fatalf("первый отказ вида не закрыл инструменты: onExhausted=%d", closed)
	}
	// Модель игнорирует закрытие на следующем модельном ходу и снова зовёт
	// инструмент — стоп-кран (B1).
	modelTurn(t, m)
	ev, err = m.OnToolExecution(next, &events.ToolCallEvent{ID: "m4", Name: "web_search"}, cx)
	if err != nil {
		t.Fatalf("четвёртый вызов: %v", err)
	}
	tr, ok := ev.(*events.ToolResultEvent)
	if !ok || !tr.Result.Final {
		t.Fatalf("игнорирование tool_choice=none не замкнуло ход: %T", ev)
	}
	if executed != 2 {
		t.Fatalf("исполнено %d, хочу 2 (только вызовы в лимите)", executed)
	}
}
