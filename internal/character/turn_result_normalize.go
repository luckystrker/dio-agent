// Нормализация необязательных полей TurnResult (этап 38.1–38.3): слабая
// модель заполняет всё, что видит в схеме, служебным мусором («null», «н/д»,
// «]», «.», «нечего сохранять») и простынями текста. Мусор = отсутствие поля,
// длинное = обрезка по рунам с «…» — до валидации, чтобы ход не терялся из-за
// необязательного поля. Обязательные структурные ошибки остаются терминальными.
package character

import (
	"log"
	"strings"
	"unicode"
)

// junkOptionalPrefixes — начала строк, которыми модель говорит «поля нет».
var junkOptionalPrefixes = []string{
	"нечего", "не используется", "не нужен", "не нужна", "не нужно",
	"не требуется", "нет стикера", "не надо", "без стикера",
}

// junkOptionalString — необязательная строка заполнена «ничем» (38.1): пусто,
// null/none/nil/н/д/нет, прочерк, только пунктуация, «нечего…», «не
// используется…». Эмодзи содержимым не считается только для стикера — там
// своя проверка (stickerLabelNone).
func junkOptionalString(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return true
	}
	switch t {
	case "null", "none", "nil", "н/д", "н.д.", "нд", "n/a", "na", "undefined", "нет", "-", "—", "–":
		return true
	}
	hasContent := 0
	for _, r := range t {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			hasContent++
		}
	}
	if hasContent < 2 {
		return true // только пунктуация/пробелы/символы или одна буква
	}
	for _, prefix := range junkOptionalPrefixes {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

// optionalText — необязательная строка после нормализации: мусор → пусто,
// иначе обрезка по рунам с «…» до max.
func optionalText(s string, max int) string {
	s = strings.TrimSpace(s)
	if junkOptionalString(s) {
		return ""
	}
	return truncateOptional(s, max)
}

// truncateOptional режет строку по рунам, добавляя «…» в пределах max.
func truncateOptional(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}

// validMessageRef — значение похоже на метку сообщения (#m<id>, допускается
// терпимый разбор ParseMessageLabel): текст в forward/reply — не метка.
func validMessageRef(s string) bool {
	if runeLen(s) > maxMessageLabelLen {
		return false
	}
	_, ok := ParseMessageLabel(s)
	return ok
}

// normalizeOptionalFields приводит необязательные строки к согласованному виду
// до валидации (38.1, 38.3): мусор → отсутствие поля, длинное → обрезка.
func normalizeOptionalFields(r TurnResult) TurnResult {
	var dropped []string

	// Заметки наблюдения: мусорные строки выбрасываются, длинные режутся.
	if len(r.Notes) > 0 {
		kept := make([]string, 0, len(r.Notes))
		for _, note := range r.Notes {
			note = strings.TrimSpace(note)
			if junkOptionalString(note) {
				dropped = append(dropped, "notes")
				continue
			}
			kept = append(kept, truncateOptional(note, maxNoteLength))
		}
		if len(kept) == 0 {
			kept = nil
		}
		r.Notes = kept
	}

	// Описания картинок: позиции не сдвигаем (37.1) — мусор становится пустой
	// фразой «картинку не описываю», а не описанием соседней картинки.
	for i := range r.ImageNotes {
		note := strings.TrimSpace(r.ImageNotes[i])
		if junkOptionalString(note) {
			note = ""
			dropped = append(dropped, "image_notes")
		}
		r.ImageNotes[i] = truncateOptional(note, maxImageNoteLength)
	}

	// Заметка сохранения стикера: «н/д»/«нечего сохранять» = сохранять нечего.
	if r.SaveSticker != nil {
		note := strings.TrimSpace(r.SaveSticker.Note)
		if junkOptionalString(note) {
			r.SaveSticker = nil
			dropped = append(dropped, "save_sticker")
		} else {
			r.SaveSticker.Note = truncateOptional(note, stickerNoteLimit)
		}
	}

	// Стикер: «null»/«.»/«]» — не запрос стикера (33.1); слишком длинное — мусор.
	r.Sticker = strings.TrimSpace(r.Sticker)
	if r.Sticker != "" && (stickerLabelNone(r.Sticker) || runeLen(r.Sticker) > maxStickerLabel) {
		r.Sticker = ""
		dropped = append(dropped, "sticker")
	}

	// Пересылка — только метка сообщения: текст/пунктуация = пересылки нет.
	r.Forward = strings.TrimSpace(r.Forward)
	if r.Forward != "" && !validMessageRef(r.Forward) {
		r.Forward = ""
		dropped = append(dropped, "forward")
	}

	// Верхнеуровневый reason и основания дельт.
	r.Reason = optionalText(r.Reason, maxReasonLength)
	if r.MoodDelta != nil {
		r.MoodDelta.Reason = optionalText(r.MoodDelta.Reason, maxReasonLength)
		if r.MoodDelta.Reason == "" {
			r.MoodDelta = nil // основание — обязательная часть дельты
			dropped = append(dropped, "mood_delta")
		}
	}
	if r.RelationshipDelta != nil {
		r.RelationshipDelta.Reason = optionalText(r.RelationshipDelta.Reason, maxReasonLength)
		if r.RelationshipDelta.Reason == "" {
			r.RelationshipDelta = nil
			dropped = append(dropped, "relationship_delta")
		}
	}

	// Намерения: мусорная/пустая тема или основание — предложение не создаётся,
	// длинные режутся (38.3), остальные намерения хода живут.
	if len(r.Intentions) > 0 {
		kept := make([]IntentionProposal, 0, len(r.Intentions))
		for _, p := range r.Intentions {
			p.Topic = optionalText(p.Topic, maxIntentionTopicLength)
			p.Reason = optionalText(p.Reason, maxReasonLength)
			if p.Topic == "" || p.Reason == "" {
				dropped = append(dropped, "intentions")
				continue
			}
			kept = append(kept, p)
		}
		if len(kept) == 0 {
			kept = nil
		}
		r.Intentions = kept
	}

	if len(dropped) > 0 {
		log.Printf("TurnResult: мусор необязательных полей вычищен: %s (38.1)",
			strings.Join(uniqueStrings(dropped), ", "))
	}
	return r
}

// uniqueStrings — список без повторов в порядке появления (для строки лога).
func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// degradeAnswerWithoutText — answer без сообщений после repair (38.2): есть
// реакция или стикер — решение становится react (стикер остаётся, картинки и
// доставка текста уходят), иначе ignore. Возвращает ok=false, если деградировать
// нечего (это не пустой answer).
func degradeAnswerWithoutText(r TurnResult) (TurnResult, bool) {
	if r.Decision != DecisionAnswer || len(r.Messages) > 0 {
		return r, false
	}
	r.Messages = nil
	// Картинка — отдельный пузырь после текста (36.2): при react её некуда.
	r.ImageURL = ""
	if r.Reaction != "" || r.Sticker != "" {
		r.Decision = DecisionReact
		return r, true
	}
	r.Decision = DecisionIgnore
	r.DeferHours = nil
	r.Forward = ""
	r.ReplyToByMessage = nil
	return r, true
}
