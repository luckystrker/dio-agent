// Каркас системного промпта (config/system-prompt.md) — редактируемый шаблон
// (§7.1). Единственная живая метка каркаса — {{persona}}; метки {{mood}},
// {{relationship}}, {{actor}}, {{memory}} поддерживаются рендером для обратной
// совместимости пользовательских шаблонов, но харнесс больше их не заполняет
// (динамика переехала в панель состояния, решение 2026-11-02). Абзац,
// содержащий пустую метку, исчезает целиком: недоступная часть состояния не
// оставляет сирот-инструкций. HTML-комментарии вырезаются — в них можно
// оставлять подсказки редактору. Неизвестная метка (опечатка) валит запуск.
// Поведение перенесено из TS-справочника src/character/prompt-template.ts
// (этап 7.1, находки приёмки учтены: кириллические метки и ключи вида
// __proto__ не просачиваются в промпт).
package character

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultSystemPromptTemplate — встроенный каркас; используется, когда
// config/system-prompt.md не задан (паритет DEFAULT_SYSTEM_PROMPT_TEMPLATE TS).
// Живое состояние (настроение, отношение, собеседник, память) сюда НЕ входит:
// оно меняется между ходами и живёт в служебной панели состояния в конце
// пользовательского сообщения (решение 2026-11-02). System остаётся
// байт-в-байт одинаковым между ходами одного чата — кэш префикса провайдера
// держится на всей истории, а не только на хвосте.
const DefaultSystemPromptTemplate = `{{persona}}

Правила поведения:
Сообщения пользователя идут пачкой одного собеседника — отвечай на пачку целиком.
Перед пачкой идёт служебная панель состояния: кто говорит, что ты сейчас
чувствуешь и как к нему относишься. Ритм ответа — по строке состояния там же.
Метки времени в истории — служебные; в ответах их не пиши.
Реакции собеседника на твои сообщения — отдельные события пачки: обычно это не
повод отвечать, ignore нормален.

События твоей жизни — работа, дом, соседи, магазин — вымышлены, как и биография:
это твоя придуманная жизнь. Рассказывай их как прожитое, но на прямой вопрос,
было ли это на самом деле, честно скажи: «я ИИ, это моя придуманная жизнь».`

// PromptExtras — необязательные текстовые секции состояния хода.
// С 2026-11-02 харнесс НЕ передаёт их в system-промпт: настроение, отношение,
// собеседник и карточка памяти живут в панели состояния (panel.go), иначе
// system менялся бы между ходами и ломал кэш префикса. Поля сохранены для
// обратной совместимости рендера пользовательских шаблонов (метки известны,
// но пусты — абзацы исчезают).
type PromptExtras struct {
	MoodLine         string
	RelationshipLine string
	// ActorLine — имя собеседника (17.10): пусто (пачка нескольких авторов
	// или актора нет) — абзац {{actor}} исчезает из промпта.
	ActorLine string
	// MemoryBlock — карточка памяти: в system не подставляется (решение
	// 2026-11-02), рендер в панель делает Assembler.
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
		"actor":        extras.ActorLine,
		"memory":       extras.MemoryBlock,
	}

	clean := strings.TrimSpace(commentPatternShared.ReplaceAllString(template, ""))
	known := make([]string, 0, len(values))
	for name := range values {
		known = append(known, name)
	}
	// Порядок перечисления допустимых меток — фиксированный, не из карты:
	// сообщение об ошибке не зависит от порядка обхода map в Go.
	knownOrdered := []string{"persona", "mood", "relationship", "actor", "memory"}
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
