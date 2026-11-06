// Страховка от брака в messages (40.4): ответ может быть валидным JSON по
// схеме TurnResult, но вместо реплик содержать мусор — обрывки JSON, имена
// полей схемы, служебные метки или одну пунктуацию (живые ходы 246–248).
// Схема TurnResult не меняется: брак ловится после разбора и чинится одним
// repair; что осталось браком — выкидывается, все брак — деградация 38.2.
//
// Осмысленный, но не по адресу текст (например, жалоба модели на «шаблонный
// текст») — не брак ФОРМЫ: его ловит отладка (40.1–40.2), а не страховка.
package character

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// garbageRepairHint — подсказка единственного repair при браке сообщений.
const garbageRepairHint = "Ты вернул валидный JSON, но в messages — не реплики для собеседника, " +
	"а мусор: обрывки JSON, имена служебных полей, метки сообщений или одна пунктуация. " +
	"Перепиши ответ: в messages — только текст для собеседника (живые реплики в чат), " +
	"без меток #m…, без имён служебных полей и без обрывков JSON; решение и состояние остаются в полях TurnResult."

// schemaFieldTokens — имена полей схемы TurnResult (40.4): если такое имя
// попало в текст реплики токеном, это скопированный служебный формат.
var schemaFieldTokens = []string{
	"decision", "messages", "notes", "memory", "reaction",
	"reply_to_by_message", "forward", "sticker", "save_sticker",
	"image_url", "image_notes", "intentions", "defer_hours",
	"mood_delta", "relationship_delta", "reason",
}

// schemaFieldRe — поле схемы как слово. \b в Go ASCII-семантики, поэтому
// «messagesекафедра» тоже ловится: после латиницы идёт кириллица — граница.
var schemaFieldRe = regexp.MustCompile(`(?i)\b(?:` + strings.Join(schemaFieldTokens, "|") + `)\b`)

// turnResultMetaRe — служебная речь модели про JSON/схему: «Укажи JSON по
// схеме Turn», «return JSON by schema» — не реплика собеседнику.
var turnResultMetaRe = regexp.MustCompile(`(?is)turnresult|\bjson\b.{0,40}(?:схем|schema)|(?:схем|schema).{0,40}\bjson\b`)

// messageLabelAnyRe — метка сообщения внутри текста (ведущую срезает
// stripLeadingMessageLabels, здесь — любая: признак скопированного формата).
var messageLabelAnyRe = regexp.MustCompile(`#m\d+`)

// jsonRunsRe — три и более JSON-знака подряд: «}}}», «]],», «":» повторы.
var jsonRunsRe = regexp.MustCompile(`[{}\[\]",:]{3,}`)

// jsonFragmentPrefixes/Suffixes — обрывки JSON в начале/конце сообщения.
var (
	jsonFragmentPrefixes = []string{`{`, `}`, `":`, `],`, `},`, `[,`, `,:`, `","`}
	jsonFragmentSuffixes = []string{`}`, `{`, `":`, `],`, `},`, `,:`, `","`}
)

// smileyRe — пунктуация, которая всё-таки реплика: смайлики и смех.
var smileyRe = regexp.MustCompile(`^(?:[)(]+|[:;=][-^'o]?[)(dDpP3]+|<3+|х+)$`)

// emojiRune — символ-эмодзи (не любая пунктуация/математический знак).
func emojiRune(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF:
		return true
	case r >= 0x2600 && r <= 0x27BF:
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
	if m := schemaFieldRe.FindString(text); m != "" {
		return fmt.Sprintf("имя поля TurnResult как токен: %s", m)
	}
	if turnResultMetaRe.MatchString(text) {
		return "служебная речь про JSON/схему"
	}
	for _, frag := range jsonFragmentPrefixes {
		if strings.HasPrefix(text, frag) {
			return "обрывок JSON в начале: " + frag
		}
	}
	for _, frag := range jsonFragmentSuffixes {
		if strings.HasSuffix(text, frag) {
			return "обрывок JSON в конце: " + frag
		}
	}
	if run := jsonRunsRe.FindString(text); run != "" {
		return "ряд JSON-знаков: " + run
	}
	if label := messageLabelAnyRe.FindString(text); label != "" {
		return "метка сообщения в тексте: " + label
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
