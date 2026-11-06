// Страховка от брака в messages (40.4): ответ может быть валидным JSON по
// схеме TurnResult, но вместо реплик содержать мусор — одну пунктуацию, имена
// полей схемы в JSON-окружении, служебные метки или приказ себе «верни JSON»
// (живые ходы 246–248). Схема TurnResult не меняется: брак ловится после
// разбора и чинится одним repair; что осталось браком — выкидывается, все
// брак — деградация 38.2.
//
// Важно: обычный ответ с кодом (`func main() {`, `x = {a: 1}`, `return nil }`,
// `[[1],[2]]`) браком НЕ считается — скобки/двоеточия сами по себе не признак
// формата (M2 ревью 2026-11-06).
//
// Осмысленный, но не по адресу текст (например, жалоба модели на «шаблонный
// текст») — не брак ФОРМЫ: его ловит отладка (40.1–40.2), а не страховка.
package character

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// garbageRepairHint — подсказка единственного repair при браке сообщений.
const garbageRepairHint = "Ты вернул валидный JSON, но в messages — не реплики для собеседника, " +
	"а мусор: обрывки JSON, имена служебных полей, метки сообщений или одна пунктуация. " +
	"Перепиши ответ: в messages — только текст для собеседника (живые реплики в чат), " +
	"без меток #m…, без имён служебных полей и без обрывков JSON; решение и состояние остаются в полях TurnResult."

// schemaFieldTokens — имена полей схемы TurnResult (40.4): если такое имя
// попало в текст реплики токеном, это скопированный служебный формат.
// Вложенные поля mood_delta/relationship_delta/намерений — тоже.
var schemaFieldTokens = []string{
	"decision", "messages", "notes", "memory", "reaction",
	"reply_to_by_message", "forward", "sticker", "save_sticker",
	"image_url", "image_notes", "intentions", "defer_hours",
	"mood_delta", "relationship_delta", "reason",
	"valence", "arousal", "socialEnergy", "affinity", "trust",
	"delay_hours", "entities", "salience", "confidence",
}

// schemaFieldRe — поле схемы как слово. \b в Go ASCII-семантики, поэтому
// «messagesекафедра» тоже ловится: после латиницы идёт кириллица — граница.
var schemaFieldRe = regexp.MustCompile(`(?i)\b(?:` + strings.Join(schemaFieldTokens, "|") + `)\b`)

// turnResultMetaRe — служебная речь модели про JSON/схему: «Укажи JSON по
// схеме Turn», «return JSON by schema» — не реплика собеседнику.
var turnResultMetaRe = regexp.MustCompile(`(?is)turnresult|\bjson\b.{0,40}(?:схем|schema)|(?:схем|schema).{0,40}\bjson\b`)

// jsonInstructionRe — прямое указание себе «верни/укажи/выдай JSON», в том
// числе без слова «схема»: «верни JSON», «Выведи JSON», «JSON only» — не
// реплика. Живая техническая речь («Мне нравится формат JSON», «Send me the
// JSON later») под шаблон не попадает: нужен императив из списка.
var jsonInstructionRe = regexp.MustCompile(`(?is)^(?:верни|укажи|выдай|пришли|выведи|сгенерируй|напиши|ответь|отдай|сформируй|return|output|respond|reply)\s+(?:только\s+)?json(?:\s+only)?\s*[.!]?$|^json\s+only\s*[.!]?$`)

// schemeMetaRe — «по схеме Turn/…» без слова JSON: тоже служебная речь.
var schemeMetaRe = regexp.MustCompile(`(?is)по\s+схеме\s+(?:turn|turnresult|json)\b`)

// stickerLabelOnlyRe — сообщение из одной метки стикера (#3) или метки
// сообщения (#m12): это не реплика.
var stickerLabelOnlyRe = regexp.MustCompile(`^#\d+$`)

// messageLabelAnyRe — метка сообщения внутри текста (ведущую срезает
// stripLeadingMessageLabels, здесь — любая: признак скопированного формата).
var messageLabelAnyRe = regexp.MustCompile(`#m\d+`)

// smileyRe — пунктуация, которая всё-таки реплика: смайлики и смех.
var smileyRe = regexp.MustCompile(`^(?:[)(]+|[:;=][-^'o]?[)(dDpP3]+|<3+|х+)$`)

// emojiRune — символ-эмодзи (не любая пунктуация/математический знак).
// Диапазоны включают эмодзи-презентацию без FE0F: ⭐ (2B50), ⏰ (23F0),
// ⌛ (231B), стрелки-эмодзи (2190–21FF).
func emojiRune(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF:
		return true
	case r >= 0x2600 && r <= 0x27BF:
		return true
	case r >= 0x2300 && r <= 0x23FF:
		return true
	case r >= 0x2B00 && r <= 0x2BFF:
		return true
	case r >= 0x2190 && r <= 0x21FF:
		return true
	case r == 0xFE0F || r == 0x200D:
		return true
	default:
		return false
	}
}

// speechContent — в сообщении есть буквы, цифры или эмодзи (не только
// пунктуация/пробелы/символы вроде «>» и «»).
func speechContent(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || emojiRune(r) {
			return true
		}
	}
	return false
}

// MessageGarbageReason объясняет, почему сообщение — брак (пусто — реплика).
// Чистая функция без побочных эффектов: ей пользуются и ход (40.4), и replay.
func MessageGarbageReason(s string) string {
	text := strings.TrimSpace(s)
	if text == "" {
		return "пустое сообщение"
	}
	if smileyRe.MatchString(text) {
		return ""
	}
	if !speechContent(text) {
		return "только пунктуация и пробелы"
	}
	if token := fieldTokenGarbage(text); token != "" {
		return fmt.Sprintf("имя поля TurnResult как токен: %s", token)
	}
	if stickerLabelOnlyRe.MatchString(text) {
		return "только метка стикера в тексте: " + text
	}
	if turnResultMetaRe.MatchString(text) || jsonInstructionRe.MatchString(text) || schemeMetaRe.MatchString(text) {
		return "служебная речь про JSON/схему"
	}
	if label := messageLabelAnyRe.FindString(text); label != "" {
		return "метка сообщения в тексте: " + label
	}
	return ""
}

// jsonNeighbor — знак рядом с токеном, выдающий скопированный формат:
// `mood_delta:`, `"messages":`, `sticker: #3`. Запятая и «=» сюда НЕ входят:
// «See the notes, please» — живая речь, а не формат.
func jsonNeighbor(r rune) bool {
	switch r {
	case ':', '{', '}', '[', ']', '"':
		return true
	default:
		return false
	}
}

// isLatin — латинская буква (включая расширения). Кириллица рядом с
// латинским полем схемы — склейка вида «messagesекафедра».
func isLatin(r rune) bool { return unicode.IsLetter(r) && r <= 0x024F }

// fieldTokenGarbage — имя поля схемы в «форматном» окружении. Живая
// английская речь не должна выкидываться: «no reason to worry» — не брак.
// Признаки формата: сообщение состоит только из имени поля (и пунктуации),
// два РАЗНЫХ поля схемы, имя с подчёркиванием (mood_delta, image_url…),
// JSON-знак рядом (`sticker:`), склейка с не-латиницей или ВСЕ ЗАГЛАВНЫЕ.
func fieldTokenGarbage(text string) string {
	matches := schemaFieldRe.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return ""
	}
	token := text[matches[0][0]:matches[0][1]]
	// Совсем без речи, кроме имён полей: «messages», «decision:», «sticker».
	if !speechContent(schemaFieldRe.ReplaceAllString(text, "")) {
		return token
	}
	distinct := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		distinct[strings.ToLower(text[m[0]:m[1]])] = struct{}{}
	}
	if len(distinct) >= 2 {
		return token
	}
	if strings.Contains(token, "_") {
		return token
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:matches[0][0]])
	next, _ := utf8.DecodeRuneInString(text[matches[0][1]:])
	if jsonNeighbor(prev) || jsonNeighbor(next) {
		return token
	}
	if (prev != 0 && unicode.IsLetter(prev) && !isLatin(prev)) ||
		(next != 0 && unicode.IsLetter(next) && !isLatin(next)) {
		return token
	}
	return ""
}

// garbageMessages — причины брака для непустого списка сообщений.
func garbageMessages(messages []string) []string {
	var out []string
	for i, m := range messages {
		if reason := MessageGarbageReason(m); reason != "" {
			out = append(out, fmt.Sprintf("messages[%d]: %s", i+1, reason))
		}
	}
	return out
}

// dropGarbageMessages выкидывает бракованные сообщения после repair (40.4):
// остальные уходят, реплаи перенумеровываются, пустой answer деградирует как
// 38.2 (react при реакции/стикере, иначе ignore).
func dropGarbageMessages(r TurnResult) (TurnResult, []string) {
	if r.Decision != DecisionAnswer || len(r.Messages) == 0 {
		return r, nil
	}
	keptIdx := make([]int, 0, len(r.Messages))
	dropped := make([]string, 0)
	for i, m := range r.Messages {
		if reason := MessageGarbageReason(m); reason != "" {
			dropped = append(dropped, fmt.Sprintf("messages[%d]: %s", i+1, reason))
			continue
		}
		keptIdx = append(keptIdx, i+1)
	}
	if len(dropped) == 0 {
		return r, nil
	}
	kept := make([]string, len(keptIdx))
	for j, idx := range keptIdx {
		kept[j] = r.Messages[idx-1]
	}
	r.Messages = kept
	r.ReplyToByMessage = remapReplies(r.ReplyToByMessage, keptIdx)
	if len(kept) == 0 {
		if degraded, ok := degradeAnswerWithoutText(r); ok {
			return degraded, dropped
		}
	}
	return r, dropped
}

// remapReplies переносит реплаи на новые номера сообщений после чистки:
// реплай выкинутого сообщения исчезает, индекс оставшегося сдвигается.
func remapReplies(replies []MessageReply, keptIdx []int) []MessageReply {
	if len(replies) == 0 {
		return nil
	}
	position := make(map[int]int, len(keptIdx))
	for j, idx := range keptIdx {
		position[idx] = j + 1
	}
	out := make([]MessageReply, 0, len(replies))
	for _, reply := range replies {
		if newIndex, ok := position[reply.Index]; ok {
			reply.Index = newIndex
			out = append(out, reply)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// GarbageRepairHint — подсказка repair при браке в messages (40.4); нужна
// фоновому пути, который чинит answer теми же правилами.
const GarbageRepairHint = garbageRepairHint

// GarbageReasons — причины брака сообщений (для лога/replay); пусто — реплики.
func GarbageReasons(messages []string) []string { return garbageMessages(messages) }

// FixGarbageMessages — страховка 40.4 для пути, где ответ уже разобран:
// если в answer есть брак, ask делает один repair (repairAllowed); результат
// repair принимается, только если это answer с чистыми репликами. Иначе
// чистится ПЕРВЫЙ ответ — валидные реплики не теряются, ошибка repair не
// роняет ход. Возвращает итог, число repair-вызовов и выкинутый брак.
func FixGarbageMessages(first TurnResult, repairAllowed bool, ask func() (TurnResult, error)) (TurnResult, int, []string) {
	if first.Decision != DecisionAnswer || len(garbageMessages(first.Messages)) == 0 {
		return first, 0, nil
	}
	if repairAllowed {
		if repaired, err := ask(); err == nil {
			cleaned, dropped := dropGarbageMessages(repaired)
			if cleaned.Decision == DecisionAnswer && len(cleaned.Messages) > 0 {
				return cleaned, 1, dropped
			}
		}
	}
	out, dropped := dropGarbageMessages(first)
	return out, 0, dropped
}
