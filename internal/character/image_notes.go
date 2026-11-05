// Описания картинок пачки (этап 37.1): модель в том же ходе возвращает по
// короткой фразе на каждую картинку, runtime сопоставляет фразы с событиями
// пачки и сохраняет в social_events.image_note. История показывает описание
// вместо плейсхолдера — модель помнит увиденное без повторной отправки байтов.
//
// Нумерация картинок — часть текста хода: «1 — фото от Данила, 2–6 — кадры
// гифки». Кадры одного видео занимают один слот (одно описание на видео):
// модель видит несколько картинок, но описывает сюжет ролика одной фразой.
package character

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Виды вложений, к которым относится описание (social_events.image_kind).
const (
	imageKindPhoto     = "photo"
	imageKindSticker   = "sticker"
	imageKindGIF       = "gif"
	imageKindVideoNote = "video_note"
)

// ImageGroup — одна позиция нумерации: событие пачки и сколько картинок
// (кадров) ушло модели из него. Count > 1 — раскадровка одного видео.
type ImageGroup struct {
	EventID int64
	Kind    string
	First   int // 1-based номер первой картинки группы
	Count   int
	// Label — человеческое описание позиции для нумерации: «фото от Данила»,
	// «кадры гифки», «превью кружка».
	Label string
}

// ImageNotesForEvents сопоставляет image_notes модели с событиями пачки: i-я
// фраза — i-й позиции нумерации (одна на видео). Пустая фраза означает «нет
// описания» и не сдвигает остальные; лишние фразы отбрасываются.
func ImageNotesForEvents(groups []ImageGroup, notes []string) []EventImageNote {
	if len(groups) == 0 || len(notes) == 0 {
		return nil
	}
	out := make([]EventImageNote, 0, len(groups))
	for i, note := range notes {
		if i >= len(groups) {
			break
		}
		note = strings.TrimSpace(note)
		if note == "" {
			continue
		}
		group := groups[i]
		out = append(out, EventImageNote{EventID: group.EventID, Kind: group.Kind, Note: note})
	}
	return out
}

// EventImageNote — описание, привязанное к событию пачки; durable в
// model_output (восстановление хода применяет те же описания без повторной
// генерации).
type EventImageNote struct {
	EventID int64  `json:"event_id"`
	Kind    string `json:"kind"`
	Note    string `json:"note"`
}

// ImageNumberingLine — пометка перед картинками: модель должна знать, какой
// номер у какой картинки. Пусто — картинок нет.
func ImageNumberingLine(groups []ImageGroup) string {
	if len(groups) == 0 {
		return ""
	}
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		if g.Count <= 1 {
			parts = append(parts, fmt.Sprintf("%d — %s", g.First, g.Label))
			continue
		}
		last := g.First + g.Count - 1
		parts = append(parts, fmt.Sprintf("%d–%d — %s (%d кадров)", g.First, last, g.Label, g.Count))
	}
	return "[Картинки в сообщении: " + strings.Join(parts, "; ") + ". Верни image_notes по одной короткой фразе на номер.]"
}

// historyMarkerRe — вид вложения в тексте события/истории.
var (
	stickerMarkerRe   = regexp.MustCompile(`\[стикер[^\]]*\]`)
	gifMarker         = "[гифка]"
	videoNoteMarkerRe = regexp.MustCompile(`\[кружок, ([^\]]+)\]`)
)

// renderIncomingContent — текст входящего события для истории с описанием
// картинки. Без описания — ровно как раньше (плейсхолдер/подпись не меняются).
func renderIncomingContent(kind, text, noteKind, note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		if kind == "photo" {
			if strings.TrimSpace(text) == "" {
				return "[фото]"
			}
			return text
		}
		return text
	}
	switch noteKind {
	case imageKindPhoto:
		marker := "[фото: " + note + "]"
		if strings.TrimSpace(text) == "" {
			return marker
		}
		return marker + " " + text
	case imageKindSticker:
		if loc := stickerMarkerRe.FindStringIndex(text); loc != nil {
			old := text[loc[0]:loc[1]]
			inner := strings.TrimSuffix(strings.TrimPrefix(old, "[стикер"), "]")
			inner = strings.TrimSpace(inner)
			replacement := "[стикер"
			if inner != "" {
				replacement += " " + inner
			}
			replacement += ": " + note + "]"
			return text[:loc[0]] + replacement + text[loc[1]:]
		}
		return "[стикер: " + note + "]" + withSpace(text)
	case imageKindGIF:
		if idx := strings.Index(text, gifMarker); idx >= 0 {
			return text[:idx] + "[гифка: " + note + "]" + text[idx+len(gifMarker):]
		}
		return "[гифка: " + note + "]" + withSpace(text)
	case imageKindVideoNote:
		if loc := videoNoteMarkerRe.FindStringSubmatchIndex(text); loc != nil {
			duration := text[loc[2]:loc[3]]
			return text[:loc[0]] + "[кружок " + duration + ": " + note + "]" + text[loc[1]:]
		}
		return "[кружок: " + note + "]" + withSpace(text)
	default:
		return text
	}
}

func withSpace(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return " " + text
}

// truncateRunesText режет текст по рунам; длинный alt страницы не должен
// раздувать историю.
func truncateRunesText(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max-1]) + "…"
}

// renderAssistantPhoto — отправленная ботом картинка в истории (этап 36.2):
// краткое описание со страницы (alt), иначе домен источника.
func renderAssistantPhoto(payload []byte) string {
	var p struct {
		SourceURL string `json:"source_url"`
		Alt       string `json:"alt"`
	}
	_ = json.Unmarshal(payload, &p)
	if alt := strings.TrimSpace(p.Alt); alt != "" {
		return "[отправил картинку: " + truncateRunesText(alt, maxImageNoteLength) + "]"
	}
	if u, err := url.Parse(strings.TrimSpace(p.SourceURL)); err == nil && u.Host != "" {
		return "[отправил картинку: " + u.Host + "]"
	}
	return "[отправил картинку]"
}
