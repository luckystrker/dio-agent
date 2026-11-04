// Устойчивый разбор JSON модели (этап 33.3). Живые логи 2026-11-03/04:
// «turnresult: invalid character '\t' in string» (bg:intention:97) — модель
// кладёт в строковый литерал сырой управляющий символ, и строгий JSON-разбор
// теряет ход целиком. Сканер ниже экранирует только символы < 0x20 ВНУТРИ
// строк (структуру и внешние пробелы не трогает) — regexp по всему тексту
// испортил бы форматирование и escape-последовательности.
package character

import (
	"fmt"
	"strings"
)

// escapeTab — таблица стандартных коротких escape-последовательностей.
var shortEscapes = map[byte]string{
	'\b': `\b`,
	'\f': `\f`,
	'\n': `\n`,
	'\r': `\r`,
	'\t': `\t`,
}

// EscapeJSONControlChars экранирует сырые управляющие символы (< 0x20) внутри
// строковых литералов JSON. Вне строк байты копируются как есть: пробелы и
// переводы строк между токенами — законный JSON. Уже экранированные пары
// («\n», «\"») остаются нетронутыми: сканер уважает обратный слэш.
func EscapeJSONControlChars(raw []byte) []byte {
	// Быстрый путь: сырых управляющих байтов нет — возвращаем исходный срез.
	needs := false
	for _, b := range raw {
		if b < 0x20 {
			needs = true
			break
		}
	}
	if !needs {
		return raw
	}
	out := make([]byte, 0, len(raw)+8)
	inString := false
	escaped := false
	const hex = "0123456789abcdef"
	for _, b := range raw {
		if !inString {
			if b == '"' {
				inString = true
			}
			out = append(out, b)
			continue
		}
		if escaped {
			// Байт после обратного слэша — часть escape-последовательности.
			out = append(out, b)
			escaped = false
			continue
		}
		switch {
		case b == '\\':
			out = append(out, b)
			escaped = true
		case b == '"':
			out = append(out, b)
			inString = false
		case b < 0x20:
			if esc, ok := shortEscapes[b]; ok {
				out = append(out, esc...)
			} else {
				out = append(out, `\u00`...)
				out = append(out, hex[b>>4], hex[b&0xf])
			}
		default:
			out = append(out, b)
		}
	}
	return out
}

// TruncatedJSON — ответ модели оборвался (unexpected EOF): чаще всего финал
// длиннее лимита токенов или провайдер оборвал поток.
func TruncatedJSON(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unexpected EOF")
}

// RepairHint — подсказка repair-попытки (§11.3): общая для разговорного хода
// и фона. Оборванный ответ просим вернуть короче, иначе модель повторяет тот
// же длинный финал и снова упирается в лимит.
func RepairHint(perr error, schema string) string {
	hint := fmt.Sprintf("Твой последний ответ не прошёл валидацию: %v. Верни строго один JSON-объект по схеме %s, без текста вокруг.", perr, schema)
	if TruncatedJSON(perr) {
		hint += " Ответ оборвался — верни короче: меньше сообщений, кандидатов памяти и текста."
	}
	return hint
}
