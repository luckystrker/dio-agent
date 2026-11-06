// Санитизация служебных меток и подписей (этап 41): метка сообщения и
// подпись картинки попадают в доверенную историю, поэтому из них вычищается
// всё, что может подделать строку истории (перевод строки, скобки меток,
// скопированные #m-метки), а из логов — подписанные query-параметры URL.
package character

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// messageLabelTokenRe — кандидат в метку сообщения: #m123, m123, #123.
var messageLabelTokenRe = regexp.MustCompile(`(?i)(?:#m|m|#)\d+`)

// stripMessageLabelsAnywhere вырезает метки сообщений из любого места текста
// (m10 ревью 2026-11-06), не калеча пользовательский текст: метка должна стоять
// отдельным токеном (не внутри URL/слова), каноническая #m<id> режется любым
// числом цифр, терпимые формы — только достаточно длинные (m<id> ≥4, #<id> ≥5
// цифр), иначе «#2024» и «M100» в живом тексте теряли бы смысл.
func stripMessageLabelsAnywhere(text string) string {
	locs := messageLabelTokenRe.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		if !messageLabelCandidate(text, loc[0], loc[1]) {
			continue
		}
		b.WriteString(text[last:loc[0]])
		last = loc[1]
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	out := repeatedHorizontalSpaceRe.ReplaceAllString(b.String(), " ")
	out = spaceBeforePunctRe.ReplaceAllString(out, "$1")
	return strings.TrimSpace(out)
}

// repeatedHorizontalSpaceRe — двойные пробелы, оставшиеся после вырезания
// метки; переводы строк (многострочное сообщение) не трогаем.
var repeatedHorizontalSpaceRe = regexp.MustCompile(`[ \t]{2,}`)

// spaceBeforePunctRe — пробел перед знаком препинания после вырезанной метки
// («привет , как ты» → «привет, как ты»).
var spaceBeforePunctRe = regexp.MustCompile(`\s+([,.;:!?])`)

// messageLabelCandidate — найденная подстрока действительно метка: стоит
// отдельным токеном, форма каноническая или достаточно длинная.
func messageLabelCandidate(text string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if !labelBoundaryBefore(r) {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if !labelBoundaryAfter(r) {
			return false
		}
	}
	token := strings.ToLower(text[start:end])
	prefix := strings.TrimRight(token, "0123456789")
	digits := len(token) - len(prefix)
	switch prefix {
	case "#m":
		return true
	case "m":
		return digits >= 4
	case "#":
		return digits >= 5
	}
	return false
}

// labelBoundaryBefore — перед меткой допустимы начало строки, пробел или
// пунктуация; «/», «.», «-», «_», «=» и буквы/цифры — часть слова или URL.
func labelBoundaryBefore(r rune) bool {
	if unicode.IsSpace(r) || unicode.IsLetter(r) || unicode.IsDigit(r) {
		return unicode.IsSpace(r)
	}
	switch r {
	case '(', '[', '{', '"', '\'', '«', '»', '…', ',', ';', ':', '!', '?':
		return true
	}
	return false
}

// labelBoundaryAfter — после метки недопустимы буквы/цифры, «_» и «/» (URL);
// точка и другая пунктуация — конец предложения, метку режем.
func labelBoundaryAfter(r rune) bool {
	return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '/')
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// sanitizeInlineNote — подпись/описание одной строкой для доверенной истории
// (M8, m11, m18): управляющие символы и переводы строк — пробел, метки
// сообщений и квадратные скобки (вид поддельной строки истории) вычищаются,
// пробелы схлопываются. Пусто — подписи нет.
func sanitizeInlineNote(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r):
			b.WriteRune(' ')
		case r == '[' || r == ']':
			// скобки маркера истории/вложения подделывать нечем
		default:
			b.WriteRune(r)
		}
	}
	s = stripMessageLabelsAnywhere(strings.Join(strings.Fields(b.String()), " "))
	return strings.TrimSpace(s)
}

// signedQueryParamRe — query-параметр подписанного URL (sig/token/key/…): в
// журнал и лог он не пишется (m14 ревью 2026-11-06).
var signedQueryParamRe = regexp.MustCompile(`(?i)([?&](?:sig|signature|token|key|api_key|apikey|access_token|auth)=)[^&\s"'<>]+`)

// redactURLSecrets затирает значения подписанных query-параметров: URL остаётся
// узнаваемым, секрет — нет.
func redactURLSecrets(text string) string {
	if !strings.ContainsAny(text, "?&") {
		return text
	}
	return signedQueryParamRe.ReplaceAllString(text, "${1}…")
}
