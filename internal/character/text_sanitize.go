// Санитизация служебных меток и подписей (этап 41): метка сообщения и
// подпись картинки попадают в доверенную историю, поэтому из них вычищается
// всё, что может подделать строку истории (перевод строки, скобки меток,
// скопированные #m-метки), а из логов — подписанные query-параметры URL.
package character

import (
	"regexp"
	"strings"
	"unicode"
)

// messageLabelTokenRe — кандидат в метку сообщения: #m123, m123, #123.
var messageLabelTokenRe = regexp.MustCompile(`(?i)(?:#m|m|#)\d+`)

// stripMessageLabelsAnywhere вырезает метки сообщений из любого места текста
// (m10 ревью 2026-11-06): модель копирует их из истории не только в начало
// сообщения. Терпимые формы (m123, #123) вырезаются только при ≥3 цифрах —
// короткий «#2» в обычном тексте остаётся пунктом списка.
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
	return strings.TrimSpace(repeatedHorizontalSpaceRe.ReplaceAllString(b.String(), " "))
}

// repeatedHorizontalSpaceRe — двойные пробелы, оставшиеся после вырезания
// метки; переводы строк (многострочное сообщение) не трогаем.
var repeatedHorizontalSpaceRe = regexp.MustCompile(`[ \t]{2,}`)

// messageLabelCandidate — найденная подстрока действительно метка: границы с
// обеих сторон не разрезают слово, форма каноническая или достаточно длинная.
func messageLabelCandidate(text string, start, end int) bool {
	if start > 0 && isWordRune(rune(text[start-1])) {
		return false
	}
	if end < len(text) && isWordRune(rune(text[end])) {
		return false
	}
	token := strings.ToLower(text[start:end])
	prefix := strings.TrimRight(token, "0123456789")
	digits := len(token) - len(prefix)
	if prefix != "#m" && digits < 3 {
		return false
	}
	return true
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
