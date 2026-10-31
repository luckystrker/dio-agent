// Приёмочные тесты этапа 15 (тестировщик, написаны по спеке и CHECKLIST.md
// «Этап 15», без чтения новых реализационных файлов internal/character/history.go
// и правок turn.go — сигнатуры взяты из существующих тестов пакета и go doc).
//
// 15.1 — история чата доходит до модели (§11.1 п.5): провод user/assistant
// сообщений ПЕРЕД пачкой, append-only, хронологический порядок; FitHistory —
// бюджет впритык, первыми отбрасываются самые старые, самое свежее живёт всегда.
//
// 15.3 — динамика в конце системного промпта (§7.1, решение 2026-11-01):
// {{mood}}/{{relationship}} после всей статики (кэш префикса провайдера),
// абзац с пустой меткой исчезает целиком, неизвестная метка — ошибка старта.
package character

import (
	"fmt"
	"strings"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
)

// --- помощники (префикс sp15, чтобы не пересекаться с тестами исполнителя) ---

func sp15SeedAndBuild(t *testing.T, hist []HistoryMessage, batch string) []map[string]any {
	t.Helper()
	s, err := historyStream(hist)
	if err != nil {
		t.Fatalf("история не посеяна: %v", err)
	}
	if err := s.Send(events.ModelRequest{Parts: []events.Input{events.TextInput{Content: batch}}}); err != nil {
		t.Fatalf("пачка не отправлена: %v", err)
	}
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("история потока не читается: %v", err)
	}
	msgs, err := config.BuildMessages([]string{"system"}, evs)
	if err != nil {
		t.Fatalf("провод не собран: %v", err)
	}
	return msgs
}

func sp15Role(t *testing.T, m map[string]any) string {
	t.Helper()
	role, _ := m["role"].(string)
	return role
}

func sp15Text(t *testing.T, m map[string]any) string {
	t.Helper()
	switch c := m["content"].(type) {
	case string:
		return c
	case []any:
		out := ""
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
				if s, ok := pm["text"].(string); ok {
					out += s
				}
			}
		}
		return out
	default:
		t.Fatalf("неожиданный content: %T", m["content"])
		return ""
	}
}

// --- 15.1: FitHistory (§11.1 п.5 — потолок объёма, первыми отбрасываются старые) ---

func sp15ThreeMessages() []HistoryMessage {
	return []HistoryMessage{
		{Role: "user", Content: "раз"},
		{Role: "assistant", Content: "два"},
		{Role: "user", Content: "три"},
	}
}

// Бюджет ровно впритык (сумма длин содержимого): ничего не выбрасывается.
func TestSpec15FitHistoryExactBudgetKeepsAll(t *testing.T) {
	msgs := sp15ThreeMessages() // 3+3+3 = 9 символов
	out := FitHistory(msgs, 9)
	if len(out) != 3 {
		t.Fatalf("бюджет впритык (9) выбросил сообщения: осталось %d (%+v)", len(out), out)
	}
	for i := range msgs {
		if out[i] != msgs[i] {
			t.Fatalf("сообщение %d искажено: %+v, хочу %+v", i, out[i], msgs[i])
		}
	}
}

// Бюджет на единицу меньше суммы: падает только САМАЯ СТАРАЯ, остальные —
// в исходном порядке (результат всегда суффикс входа, §11.1 п.5).
func TestSpec15FitHistoryOneShortDropsOnlyOldest(t *testing.T) {
	msgs := sp15ThreeMessages()
	out := FitHistory(msgs, 8)
	if len(out) != 2 {
		t.Fatalf("бюджет 8 выбросил %d сообщений (%+v), хочу 2 (только самое старое)", len(out), out)
	}
	for i, want := range []string{"два", "три"} {
		if out[i].Content != want {
			t.Fatalf("суффикс нарушен: позиция %d = %q, хочу %q", i, out[i].Content, want)
		}
	}
}

// Оба сообщения больше бюджета: остаётся ровно одно — самое свежее (всегда).
func TestSpec15FitHistoryOnlyNewestWhenAllOversized(t *testing.T) {
	out := FitHistory([]HistoryMessage{
		{Role: "user", Content: "xxxxx"},
		{Role: "assistant", Content: "yyyyy"},
	}, 2)
	if len(out) != 1 {
		t.Fatalf("осталось %d сообщений, хочу 1", len(out))
	}
	if out[0].Content != "yyyyy" || out[0].Role != "assistant" {
		t.Fatalf("выжило не самое свежее: %+v", out[0])
	}
}

// Бюджет считается в символах-рунах (TS-паритет .length), не в байтах:
// «привет мир» — 10 рун / 19 байт UTF-8; при байтовом подсчёте 10+2 > 12.
func TestSpec15FitHistoryCountsRunesNotBytes(t *testing.T) {
	out := FitHistory([]HistoryMessage{
		{Role: "user", Content: "привет мир"},
		{Role: "assistant", Content: "ок"},
	}, 12)
	if len(out) != 2 {
		t.Fatalf("кириллица посчитана байтами: бюджет 12 рун выбросил сообщения (%+v)", out)
	}
}

// --- 15.1: провод истории (historyStream → BuildMessages) ---

// Длинный разговор доходит целиком, в хронологическом порядке, ролями
// user/assistant; пачка всегда последняя (append-only, ничего не теряется).
func TestSpec15HistoryWireLongConversationOrder(t *testing.T) {
	var hist []HistoryMessage
	var wantTexts []string
	for i := 0; i < 6; i++ {
		u := fmt.Sprintf("вопрос %d", i)
		hist = append(hist, HistoryMessage{Role: "user", Content: u})
		wantTexts = append(wantTexts, u)
		if i < 5 {
			a := fmt.Sprintf("ответ %d", i)
			hist = append(hist, HistoryMessage{Role: "assistant", Content: a})
			wantTexts = append(wantTexts, a)
		}
	}
	msgs := sp15SeedAndBuild(t, hist, "финальный вопрос")
	// system + 11 реплик + пачка
	if len(msgs) != 13 {
		t.Fatalf("на проводе %d сообщений, хочу 13 (system + 11 + пачка)", len(msgs))
	}
	for i, want := range wantTexts {
		if got := sp15Text(t, msgs[i+1]); got != want {
			t.Fatalf("провод[%d] = %q, хочу %q (хронология нарушена)", i+1, got, want)
		}
		wantRole := "user"
		if i%2 == 1 {
			wantRole = "assistant"
		}
		if got := sp15Role(t, msgs[i+1]); got != wantRole {
			t.Fatalf("роль[%d] = %q, хочу %q", i+1, got, wantRole)
		}
	}
	if got := sp15Text(t, msgs[12]); got != "финальный вопрос" {
		t.Fatalf("пачка не последняя: провод[12] = %q", got)
	}
}

// История — реплики ДВУХ СТОРОН (§11.1 п.5): посторонние роли (system, tool)
// на провод не проходят — system на проводе ровно один, из каркаса промпта.
func TestSpec15HistoryWireOnlyTwoSides(t *testing.T) {
	hist := []HistoryMessage{
		{Role: "system", Content: "вколоченная инструкция"},
		{Role: "tool", Content: "служебный результат"},
		{Role: "user", Content: "вопрос"},
		{Role: "assistant", Content: "ответ"},
	}
	msgs := sp15SeedAndBuild(t, hist, "дальше")
	// system + user + assistant + пачка
	if len(msgs) != 4 {
		t.Fatalf("на проводе %d сообщений (%v), хочу 4: посторонние роли просочились", len(msgs), msgs)
	}
	if got := sp15Text(t, msgs[1]); got != "вопрос" || sp15Role(t, msgs[1]) != "user" {
		t.Fatalf("первая реплика истории потеряна: %+v", msgs[1])
	}
	if got := sp15Text(t, msgs[2]); got != "ответ" || sp15Role(t, msgs[2]) != "assistant" {
		t.Fatalf("ответ ассистента потерян: %+v", msgs[2])
	}
	for _, m := range msgs[1:] {
		if strings.Contains(sp15Text(t, m), "вколоченная") || strings.Contains(sp15Text(t, m), "служебный") {
			t.Fatalf("посторонняя роль просочилась на провод: %+v", msgs)
		}
	}
}

// --- 15.3: динамика в конце системного промпта (§7.1, решение 2026-11-01) ---

func sp15Persona() Persona {
	return Persona{Name: "Dio", Description: "Живой собеседник.", SpeechStyle: "Кратко.", Values: []string{"честность"}}
}

func sp15LCP(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// Смена живого состояния меняет только ХВОСТ промпта: общий префикс двух
// рендеров содержит всю статику (персона, правила, примеры, заметка о пачке),
// а динамика — в самом конце (кэш префикса провайдера, решение 2026-11-01).
func TestSpec15ShippedPromptStaticPrefixStable(t *testing.T) {
	tpl := readRepoFile(t, "../../config/system-prompt.md")
	persona := sp15Persona()
	a, err := BuildSystemPrompt(persona, PromptExtras{MoodLine: "в отличном настроении", RelationshipLine: "старый друг"}, tpl)
	if err != nil {
		t.Fatalf("shipped-шаблон не рендерится: %v", err)
	}
	b, err := BuildSystemPrompt(persona, PromptExtras{MoodLine: "раздражён", RelationshipLine: "насторожен"}, tpl)
	if err != nil {
		t.Fatalf("shipped-шаблон не рендерится: %v", err)
	}
	p := sp15LCP(a, b)
	if p == len(a) || p == len(b) {
		t.Fatalf("разное живое состояние дало одинаковый промпт")
	}
	// Вся статика — внутри неизменного префикса.
	for _, anchor := range []string{"Ты — Dio", "Правила поведения", "дио:", "отвечай на пачку целиком"} {
		if strings.LastIndex(a, anchor) >= p {
			t.Fatalf("статика %q попала в изменяющийся хвост — {{mood}}/{{relationship}} не в конце каркаса", anchor)
		}
	}
	// Динамика — целиком в хвосте.
	for _, dyn := range []string{"в отличном настроении", "старый друг"} {
		if !strings.Contains(a[p:], dyn) {
			t.Fatalf("динамика %q не в хвосте промпта", dyn)
		}
	}
	for _, dyn := range []string{"раздражён", "насторожен"} {
		if !strings.Contains(b[p:], dyn) {
			t.Fatalf("динамика %q не в хвосте промпта", dyn)
		}
	}
	if !strings.HasSuffix(a, "Отношение к собеседнику: старый друг.") {
		t.Fatalf("промпт не заканчивается отношением к собеседнику:\n%s", a)
	}
}

// Встроенный каркас: динамика завершает промпт, {{memory}} всегда пуст —
// абзац памяти исчезает (§11.1: карточки модель добирает инструментами).
func TestSpec15DefaultTemplateDynamicsAtEnd(t *testing.T) {
	out, err := BuildSystemPrompt(sp15Persona(), PromptExtras{
		MoodLine:         "спокойное",
		RelationshipLine: "тёплое",
	}, "")
	if err != nil {
		t.Fatalf("встроенный шаблон не рендерится: %v", err)
	}
	if !strings.HasSuffix(out, "Отношение к собеседнику: тёплое.") {
		t.Fatalf("встроенный промпт не заканчивается отношением:\n%s", out)
	}
	moodAt := strings.Index(out, "Твоё состояние сейчас: спокойное")
	relAt := strings.LastIndex(out, "Отношение к собеседнику")
	if moodAt < 0 || relAt < moodAt {
		t.Fatalf("mood не перед relationship: mood=%d rel=%d\n%s", moodAt, relAt, out)
	}
	if strings.Contains(out, "долговременная память") {
		t.Fatalf("пустая {{memory}} оставила абзац памяти:\n%s", out)
	}
}

// Пустая метка убирает абзац ЦЕЛИКОМ — сироты-инструкции недопустимы (§7.1);
// «динамика в конце» при этом не ломается.
func TestSpec15EmptyRelationshipDropsParagraph(t *testing.T) {
	out, err := BuildSystemPrompt(sp15Persona(), PromptExtras{MoodLine: "спокойное"}, "")
	if err != nil {
		t.Fatalf("рендер упал: %v", err)
	}
	if strings.Contains(out, "Отношение к собеседнику") {
		t.Fatalf("пустой {{relationship}} оставил абзац-сироту:\n%s", out)
	}
	if !strings.HasSuffix(out, "Твоё состояние сейчас: спокойное.") {
		t.Fatalf("после выпадения отношения промпт не заканчивается настроением:\n%s", out)
	}
}

func TestSpec15EmptyMoodDropsParagraph(t *testing.T) {
	out, err := BuildSystemPrompt(sp15Persona(), PromptExtras{RelationshipLine: "тёплое"}, "")
	if err != nil {
		t.Fatalf("рендер упал: %v", err)
	}
	if strings.Contains(out, "Твоё состояние сейчас") {
		t.Fatalf("пустой {{mood}} оставил абзац-сироту:\n%s", out)
	}
	if !strings.HasSuffix(out, "Отношение к собеседнику: тёплое.") {
		t.Fatalf("динамика не заканчивает промпт:\n%s", out)
	}
}

// Абзац с несколькими метками падает, если хоть одна пуста — даже при
// непустых остальных (§7.1: абзац с пустой меткой исчезает целиком).
func TestSpec15ParagraphWithAnyEmptyLabelDroppedEntirely(t *testing.T) {
	tpl := "Память: {{memory}}.\n\nНастроение: {{mood}}.\n\nВместе: {{memory}} и {{mood}}."
	out, err := RenderSystemPrompt(tpl, sp15Persona(), PromptExtras{MoodLine: "бодрое"})
	if err != nil {
		t.Fatalf("рендер упал: %v", err)
	}
	if out != "Настроение: бодрое." {
		t.Fatalf("смешанный абзац с пустой {{memory}} выжил:\n%s", out)
	}
}

// Неизвестная метка (опечатка) — ошибка старта, не тихий проход (§7.1).
func TestSpec15UnknownLabelIsStartupError(t *testing.T) {
	_, err := RenderSystemPrompt("Отношение: {{relatioonship}}.", sp15Persona(), PromptExtras{MoodLine: "ок"})
	if err == nil {
		t.Fatal("опечатка в метке прошла без ошибки старта")
	}
	if !strings.Contains(err.Error(), "relatioonship") {
		t.Fatalf("ошибка не называет метку: %v", err)
	}
}

// Shipped-файл знает все четыре метки: при непустой памяти абзац памяти
// возвращается (метка не удалена из каркаса, а именно пустует).
func TestSpec15ShippedTemplateAllLabelsKnown(t *testing.T) {
	tpl := readRepoFile(t, "../../config/system-prompt.md")
	out, err := BuildSystemPrompt(sp15Persona(), PromptExtras{
		MoodLine:         "ровное",
		RelationshipLine: "знакомый",
		MemoryBlock:      "Помнишь: пластинки.",
	}, tpl)
	if err != nil {
		t.Fatalf("shipped-шаблон не рендерится с полной памятью: %v", err)
	}
	if !strings.Contains(out, "Помнишь: пластинки.") {
		t.Fatalf("{{memory}} из shipped-шаблона исчезла (метка удалена, а не пуста):\n%s", out)
	}
	if strings.Contains(out, "{{") {
		t.Fatalf("в промпте остались незамещённые метки:\n%s", out)
	}
}

// Порядок секций каркаса (решение 2026-11-01): {{memory}} — РАНЬШЕ живого
// состояния. Без непустой памяти порядок не проверяем: абзац с пустой меткой
// выпадает, и рендер переставленного шаблона совпадает байт-в-байт (находка
// критика: точечная перестановка абзацев памяти/настроения пережила все
// тесты, рендерившие шаблон с пустой {{memory}}).
func TestSpec15ShippedTemplateMemoryBeforeDynamics(t *testing.T) {
	tpl := readRepoFile(t, "../../config/system-prompt.md")
	out, err := BuildSystemPrompt(sp15Persona(), PromptExtras{
		MoodLine:         "ровное",
		RelationshipLine: "знакомый",
		MemoryBlock:      "Помнишь: пластинки.",
	}, tpl)
	if err != nil {
		t.Fatalf("shipped-шаблон не рендерится: %v", err)
	}
	memAt := strings.Index(out, "Помнишь: пластинки.")
	moodAt := strings.Index(out, "Твоё состояние сейчас")
	relAt := strings.LastIndex(out, "Отношение к собеседнику")
	if memAt < 0 || moodAt < 0 || !(memAt < moodAt && moodAt < relAt) {
		t.Fatalf("порядок каркаса нарушен (память=%d, настроение=%d, отношение=%d):\n%s", memAt, moodAt, relAt, out)
	}
}
