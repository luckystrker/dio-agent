// Каркас системного промпта (config/system-prompt.md) — редактируемый шаблон
// (§7.1). Метки {{persona}}, {{mood}}, {{relationship}}, {{memory}} заменяются
// на состояние из БД. Абзац, содержащий пустую метку, исчезает целиком:
// недоступная часть состояния не оставляет сирот-инструкций. HTML-комментарии
// вырезаются — в них можно оставлять подсказки редактору. Неизвестная метка
// (опечатка) валит запуск. Поведение перенесено из TS-справочника
// src/character/prompt-template.ts (этап 7.1, находки приёмки учтены:
// кириллические метки и ключи вида __proto__ не просачиваются в промпт).
package character

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultSystemPromptTemplate — встроенный каркас; используется, когда
// config/system-prompt.md не задан (паритет DEFAULT_SYSTEM_PROMPT_TEMPLATE TS).
// Живое состояние ({{mood}}/{{relationship}}) — в самом конце: статика остаётся
// байт-в-байт между ходами, смена банды перекэширует у провайдера только хвост.
const DefaultSystemPromptTemplate = `{{persona}}

Твоя долговременная память об этом чате (используй естественно, не выдумывай сверх записанного и не цитируй формат):
{{memory}}

Сообщения пользователя идут пачкой одного собеседника — отвечай на пачку целиком.

Твоё состояние сейчас: {{mood}}.

Отношение к собеседнику: {{relationship}}.`

// PromptExtras — необязательные текстовые секции состояния хода.
type PromptExtras struct {
	MoodLine         string
	RelationshipLine string
	// MemoryBlock — всегда пуст в базовом контексте v0.7 (§11.1): карточки
	// памяти модель добирает сама инструментами §10.4, а не заранее.
	MemoryBlock string
}

// RenderPersonaBlock — текстовая персона-блок: кто я, манера речи, ценности,
// интересы, заданная биография.
func RenderPersonaBlock(p Persona) string {
	lines := []string{
		fmt.Sprintf("Ты — %s. %s", p.Name, p.Description),
		fmt.Sprintf("Манера речи: %s", p.SpeechStyle),
		fmt.Sprintf("Ценности: %s.", strings.Join(p.Values, ", ")),
	}
	if len(p.Interests) > 0 {
		lines = append(lines, fmt.Sprintf("Темы, о которых тебе интересно говорить: %s.", strings.Join(p.Interests, ", ")))
	}
	if p.SeedBio != nil {
		lines = append(lines, fmt.Sprintf("Твоя биография: %s", *p.SeedBio))
	}
	return strings.Join(lines, "\n\n")
}

// placeholderPattern — содержимое между двойными фигурными скобками; имя
// обрезается по краям. Широкий паттерн вместо \w+: метка с кириллицей или
// опечаткой тоже должна попасть в валидацию, а не просочиться в промпт сырым
// текстом (находка приёмки 7.1).
var placeholderPattern = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

// commentPatternShared — HTML-комментарии шаблона и persona-файла.
var commentPatternShared = regexp.MustCompile(`(?s)<!--.*?-->`)

// paragraphSplitPattern — граница абзаца: перевод строки, пробелы, перевод
// строки (поведение /\n\s*\n/ в TS).
var paragraphSplitPattern = regexp.MustCompile(`\n\s*\n`)

func placeholderNames(text string) []string {
	var names []string
	for _, match := range placeholderPattern.FindAllStringSubmatch(text, -1) {
		names = append(names, strings.TrimSpace(match[1]))
	}
	return names
}

// RenderSystemPrompt заменяет метки шаблона; бросает ошибку при неизвестной
// метке (опечатка в шаблоне останавливает запуск, §7.1).
func RenderSystemPrompt(template string, persona Persona, extras PromptExtras) (string, error) {
	// Значения — карта известных имён: лишние ключи в неё не попадут.
	values := map[string]string{
		"persona":      RenderPersonaBlock(persona),
		"mood":         extras.MoodLine,
		"relationship": extras.RelationshipLine,
		"memory":       extras.MemoryBlock,
	}

	clean := strings.TrimSpace(commentPatternShared.ReplaceAllString(template, ""))
	known := make([]string, 0, len(values))
	for name := range values {
		known = append(known, name)
	}
	// Порядок перечисления допустимых меток — фиксированный, не из карты:
	// сообщение об ошибке не зависит от порядка обхода map в Go.
	knownOrdered := []string{"persona", "mood", "relationship", "memory"}
	for _, name := range placeholderNames(clean) {
		if _, ok := values[name]; !ok {
			return "", fmt.Errorf(
				"шаблон системного промпта: неизвестная метка {{%s}} — допустимы: %s",
				name, strings.Join(knownOrdered, ", "))
		}
	}

	paragraphs := paragraphSplitPattern.Split(clean, -1)
	kept := make([]string, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		if strings.TrimSpace(paragraph) == "" {
			continue
		}
		// Абзац с пустой меткой исчезает целиком.
		drop := false
		for _, name := range placeholderNames(paragraph) {
			if values[name] == "" {
				drop = true
				break
			}
		}
		if drop {
			continue
		}
		kept = append(kept, placeholderPattern.ReplaceAllStringFunc(paragraph, func(m string) string {
			name := strings.TrimSpace(placeholderPattern.FindStringSubmatch(m)[1])
			return values[name]
		}))
	}
	return strings.TrimSpace(strings.Join(kept, "\n\n")), nil
}

// BuildSystemPrompt — явная сборка system-промпта из БД-состояния (§7, §11.1):
// один источник истины; без шаблона (пустая строка) — встроенный по умолчанию.
func BuildSystemPrompt(persona Persona, extras PromptExtras, template string) (string, error) {
	if strings.TrimSpace(template) == "" {
		template = DefaultSystemPromptTemplate
	}
	return RenderSystemPrompt(template, persona, extras)
}
