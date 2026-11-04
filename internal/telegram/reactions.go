// Реакции пользователя на сообщения бота (34.1): текст события с цитатой
// ответа и поиск цели в delivery_outbox. Нормализация отдельная (normalize.go):
// цитату знает только БД (outbox sent), поэтому текст собирает ingest.
package telegram

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
)

// reactionQuoteLimit — потолок цитаты ответа бота в тексте события (34.1):
// достаточно, чтобы модель узнала, на что среагировали.
const reactionQuoteLimit = 120

// reactionEventText — событие для хода и истории:
// «[поставил 😂 на твоё «…»]» / «[убрал реакцию 😂 с твоего «…»]»; цель
// неизвестна (реакция на чужое сообщение) — без цитаты.
func reactionEventText(emoji string, added bool, quote string) string {
	verb := "поставил"
	preposition := " на твоё "
	if !added {
		verb = "убрал реакцию"
		preposition = " с твоего "
	}
	if emoji != "" {
		verb += " " + emoji
	}
	if quote = strings.TrimSpace(quote); quote != "" {
		return "[" + verb + preposition + "«" + truncateRunes(quote, reactionQuoteLimit) + "»]"
	}
	return "[" + verb + "]"
}

// truncateRunes режет строку по рунам с многоточием.
func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// reactionTargetText ищет в outbox доставленный ответ бота (sent) по
// chat_id + telegram message_id и возвращает цитату. found=false — сообщение
// не бота (или доставка не подтверждена): реакция остаётся journal_only.
func reactionTargetText(ctx context.Context, db interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, chatID, messageID int64) (quote string, found bool, err error) {
	var raw []byte
	err = db.QueryRow(ctx, `
		SELECT payload FROM delivery_outbox
		WHERE chat_id = $1 AND telegram_message_id = $2 AND status = 'sent'
		  AND payload->>'kind' IN ('message', 'sticker')
		ORDER BY id DESC LIMIT 1`, chatID, messageID).Scan(&raw)
	if err != nil {
		if isNoRowsError(err) {
			return "", false, nil
		}
		return "", false, err
	}
	var payload struct {
		Kind  string `json:"kind"`
		Text  string `json:"text"`
		Emoji string `json:"emoji"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", false, nil // битый payload — цель не определена
	}
	if payload.Kind == "sticker" {
		if payload.Emoji != "" {
			return "[стикер " + payload.Emoji + "]", true, nil
		}
		return "[стикер]", true, nil
	}
	text := strings.TrimSpace(payload.Text)
	if text == "" {
		return "", true, nil
	}
	return truncateRunes(text, reactionQuoteLimit), true, nil
}
