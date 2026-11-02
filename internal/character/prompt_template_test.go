// Юнит-тесты рендера системного промпта: паритет src/character/prompt-template.ts
// (этап 7.1): пустая метка убирает абзац целиком, неизвестная метка — ошибка,
// комментарии вырезаются, кириллическая метка тоже валидируется.
package character

import (
	"strings"
	"testing"
)

func testPersona() Persona {
	bio := "Вырос в порту."
	return Persona{
		Version:      1,
		Name:         "Dio",
		Description:  "Живой собеседник.",
		SpeechStyle:  "Кратко.",
		Values:       []string{"честность", "прямота"},
		MoodBaseline: Mood{Valence: 0.1, Arousal: 0.4, SocialEnergy: 0.6},
		SeedBio:      &bio,
	}
}

func TestRenderPersonaBlock(t *testing.T) {
	block := RenderPersonaBlock(testPersona())
	for _, want := range []string{"Ты — Dio. Живой собеседник.", "Манера речи: Кратко.", "Ценности: честность, прямота.", "Твоя биография: Вырос в порту."} {
		if !strings.Contains(block, want) {
			t.Fatalf("в персона-блоке нет %q:\n%s", want, block)
		}
	}
}

func TestRenderPersonaBlockInterests(t *testing.T) {
	// Интересы (§7.1, решение 2026-10-31): с секцией — строка тем; без —
	// сироты-строки нет.
	withInterests := testPersona()
	withInterests.Interests = []string{"старое железо", "море"}
	block := RenderPersonaBlock(withInterests)
	if !strings.Contains(block, "Темы, о которых тебе интересно говорить: старое железо, море.") {
		t.Fatalf("интересы не в персона-блоке:\n%s", block)
	}
	without := RenderPersonaBlock(testPersona())
	if strings.Contains(without, "интересно говорить") {
		t.Fatalf("без интересов строка тем не должна рендериться:\n%s", without)
	}
}

func TestRenderSystemPromptEmptyLabelDropsParagraph(t *testing.T) {
	// {{relationship}} пуст — весь абзац про отношение исчезает.
	template := "{{persona}}\n\nОтношение к собеседнику: {{relationship}}.\n\nТвоё состояние: {{mood}}."
	out, err := RenderSystemPrompt(template, testPersona(), PromptExtras{MoodLine: "в ровном настроении"})
	if err != nil {
		t.Fatalf("рендер упал: %v", err)
	}
	if strings.Contains(out, "Отношение к собеседнику") {
		t.Fatalf("абзац с пустой меткой не исчез:\n%s", out)
	}
	if !strings.Contains(out, "в ровном настроении") || !strings.Contains(out, "Ты — Dio") {
		t.Fatalf("заполненные абзацы потерялись:\n%s", out)
	}
}

func TestRenderSystemPromptUnknownLabelFails(t *testing.T) {
	_, err := RenderSystemPrompt("Привет, {{moodz}}!", testPersona(), PromptExtras{})
	if err == nil || !strings.Contains(err.Error(), "неизвестная метка {{moodz}}") {
		t.Fatalf("хочу ошибку про неизвестную метку, получено: %v", err)
	}
}

func TestRenderSystemPromptCyrillicLabelValidated(t *testing.T) {
	// Метка с кириллицей или опечаткой не просачивается сырым текстом (находка приёмки 7.1).
	_, err := RenderSystemPrompt("Секция {{настроение}} в деле", testPersona(), PromptExtras{})
	if err == nil || !strings.Contains(err.Error(), "неизвестная метка") {
		t.Fatalf("кириллическая метка не поймана: %v", err)
	}
}

func TestRenderSystemPromptCommentsStripped(t *testing.T) {
	template := "<!-- подсказка -->\n{{persona}}\n<!-- вторая -->"
	out, err := RenderSystemPrompt(template, testPersona(), PromptExtras{})
	if err != nil {
		t.Fatalf("рендер упал: %v", err)
	}
	if strings.Contains(out, "подсказка") || strings.Contains(out, "вторая") {
		t.Fatalf("комментарии попали в промпт:\n%s", out)
	}
}

func TestRenderSystemPromptEmptyMemoryLabel(t *testing.T) {
	// v0.7 §11.1: {{memory}} в базовом контексте пуст — абзац исчезает,
	// память модель добирает инструментами.
	out, err := RenderSystemPrompt(DefaultSystemPromptTemplate, testPersona(),
		PromptExtras{MoodLine: "спокоен", RelationshipLine: "почти незнаком"})
	if err != nil {
		t.Fatalf("рендер упал: %v", err)
	}
	if strings.Contains(out, "{{memory}}") || strings.Contains(out, "долговременная память") {
		t.Fatalf("пустой {{memory}} оставил сироту-инструкцию:\n%s", out)
	}
}

func TestBuildSystemPromptDefaultTemplate(t *testing.T) {
	// Пустой шаблон = встроенный по умолчанию.
	out, err := BuildSystemPrompt(testPersona(), PromptExtras{MoodLine: "спокоен"}, "")
	if err != nil {
		t.Fatalf("рендер упал: %v", err)
	}
	if !strings.Contains(out, "Сообщения пользователя идут пачкой") {
		t.Fatalf("шаблон по умолчанию не применён:\n%s", out)
	}
}

func TestBuildSystemPromptFileTemplate(t *testing.T) {
	out, err := BuildSystemPrompt(testPersona(), PromptExtras{MoodLine: "спокоен"}, "Каркас: {{mood}}")
	if err != nil {
		t.Fatalf("рендер упал: %v", err)
	}
	if out != "Каркас: спокоен" {
		t.Fatalf("файловый шаблон отрендерен неверно: %q", out)
	}
}

// TestRenderSystemPromptActorLabelSupported — метка {{actor}} поддерживается
// рендером для старых пользовательских шаблонов, но в дефолтном каркасе её
// больше нет (решение 2026-11-02: собеседник — в панели состояния).
func TestRenderSystemPromptActorLabelSupported(t *testing.T) {
	out, err := RenderSystemPrompt(DefaultSystemPromptTemplate, testPersona(), PromptExtras{
		MoodLine:  "в ровном настроении",
		ActorLine: "Коля",
	})
	if err != nil {
		t.Fatalf("рендер упал: %v", err)
	}
	if strings.Contains(out, "Ты говоришь с") || strings.Contains(out, "{{actor}}") {
		t.Fatalf("дефолтный каркас не должен содержать собеседника: %s", out)
	}
	out, err = RenderSystemPrompt("Собеседник: {{actor}}.", testPersona(), PromptExtras{ActorLine: "Аня"})
	if err != nil {
		t.Fatalf("метка {{actor}} не распознана в своём шаблоне: %v", err)
	}
	if !strings.Contains(out, "Собеседник: Аня.") {
		t.Fatalf("{{actor}} не заменён: %s", out)
	}
}

// TestDefaultSystemPromptStatic — решение 2026-11-02: живое состояние
// (настроение, отношение, собеседник, память) больше не входит в system.
// Рендер с разными extras даёт байт-в-байт одинаковый текст: кэш префикса
// провайдера не ломается при смене настроения.
func TestDefaultSystemPromptStatic(t *testing.T) {
	first, err := RenderSystemPrompt(DefaultSystemPromptTemplate, testPersona(), PromptExtras{
		MoodLine:         "взволнован",
		RelationshipLine: "близкий друг",
		ActorLine:        "Коля",
		MemoryBlock:      "любит пластинки",
	})
	if err != nil {
		t.Fatalf("рендер упал: %v", err)
	}
	second, err := RenderSystemPrompt(DefaultSystemPromptTemplate, testPersona(), PromptExtras{
		MoodLine:         "спокоен и ленив",
		RelationshipLine: "едва знаком",
		ActorLine:        "Аня",
		MemoryBlock:      "другое",
	})
	if err != nil {
		t.Fatalf("рендер упал: %v", err)
	}
	if first != second {
		t.Fatalf("system меняется вместе с состоянием:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	for _, dynamic := range []string{"взволнован", "спокоен", "Коля", "Аня", "близкий друг", "пластинки"} {
		if strings.Contains(first, dynamic) {
			t.Fatalf("динамическое %q попало в system:\n%s", dynamic, first)
		}
	}
}
