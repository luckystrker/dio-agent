// Страховки отложенных намерений (26.2): модель путает часы с минутами
// (боевая база: check_in 0.094 ч = 5.6 минуты, share 0.069 ч) и кладёт в тему
// мусор-плейсхолдер. Runtime не отклоняет намерение целиком, а поднимает
// задержку до пола по виду и отсеивает мусорную тему — ход живёт (PHILOSOPHY
// п.7: модель предлагает, runtime решает).
package character

import (
	"strings"
	"unicode"
)

// DelayHoursHint — единый текст единиц измерения для всех мест, где модель
// выбирает задержку: constraints хода, описание JSON-схемы, constraints фона.
const DelayHoursHint = "delay_hours — в ЧАСАХ: 0.5 = 30 минут, 3 = три часа"

// Runtime-пол задержки по виду намерения (часы). Смысл: «как ты» через 5–20
// минут — не забота, а путаница часов с минутами; share раньше 15 минут —
// находка ещё не осела; reply_later раньше 6 минут — не «позже».
const (
	minCheckInDelayHours    = 2.0
	minShareDelayHours      = 0.25
	minReplyLaterDelayHours = 0.1
)

// floorIntentionDelay поднимает задержку до пола вида. raised=true — значение
// изменено (нужно для лога оператора); неизвестный вид не трогается.
func floorIntentionDelay(kind string, hours float64) (float64, bool) {
	var floor float64
	switch kind {
	case "check_in":
		floor = minCheckInDelayHours
	case "share":
		floor = minShareDelayHours
	case "reply_later":
		floor = minReplyLaterDelayHours
	default:
		return hours, false
	}
	if hours < floor {
		return floor, true
	}
	return hours, false
}

// garbageTopicReason — тема-мусор (пустая, без букв, шаблон-плейсхолдер):
// причина отказа для лога; пустая строка — тема годная. Отклоняется только
// предложение намерения, ход продолжается (как дубль, §14.1).
func garbageTopicReason(topic string) string {
	trimmed := strings.TrimSpace(topic)
	if trimmed == "" {
		return "пустая тема"
	}
	if !hasLetter(trimmed) {
		return "нет ни одной буквы (эмодзи/пунктуация)"
	}
	normalized := strings.ToLower(trimmed)
	switch normalized {
	case "тема", "topic", "todo", "xxx":
		return "шаблон-плейсхолдер"
	}
	if strings.HasPrefix(normalized, "placeholder") || strings.HasPrefix(normalized, "todo") {
		return "шаблон-плейсхолдер"
	}
	// «xxxx», «xxxxx» и т.п. — тот же плейсхолдер.
	if strings.Trim(normalized, "x") == "" {
		return "шаблон-плейсхолдер"
	}
	return ""
}

// hasLetter — есть ли в строке хотя бы одна буква (любой алфавит).
func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}
