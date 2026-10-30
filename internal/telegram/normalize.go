// Нормализация Telegram update → событие (§6.1, шаг 1). Чистые функции:
// безопасное извлечение метаданных из raw JSON. Поведение — src/telegram/normalize.ts.
//
// Нормализация падает только если update целиком неразборчив (нет
// update_id) — такое не записать даже в журнал. Неподдерживаемые типы
// (callback_query и т.п.) возвращаются с Supported=false: фиксируются как
// journal_only, чтобы offset не застревал (§6.1).
package telegram

import (
	"encoding/json"
	"fmt"
)

// ChatType — тип чата события (§6.2 различает окна лички и группы).
type ChatType string

const (
	ChatPrivate ChatType = "private"
	ChatGroup   ChatType = "group"
)

// EventKind — нормализованный тип события (колонка social_events.kind).
type EventKind string

const (
	KindMessage EventKind = "message"
	KindReply   EventKind = "reply"
	KindMention EventKind = "mention"
	KindCommand EventKind = "command"
	KindEdited  EventKind = "edited"
	KindOther   EventKind = "other"
)

// NormalizedEvent — событие, извлечённое из raw update.
type NormalizedEvent struct {
	TelegramUpdateID int64
	ChatID           int64
	ChatType         ChatType
	ActorID          *int64
	// ActorName — отображаемое имя автора (first_name/username).
	ActorName *string
	Kind      EventKind
	Text      *string
	// Supported=false — update не поддерживается: журнал без обработки.
	Supported bool
}

// telegramEntity, telegramMessage, telegramUpdateShape — узкое представление
// нужных полей raw update; всё остальное остаётся в raw_update (jsonb).
type telegramEntity struct {
	Type string `json:"type"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type telegramMessage struct {
	From           *telegramUser    `json:"from"`
	Text           *string          `json:"text"`
	Entities       []telegramEntity `json:"entities"`
	ReplyToMessage json.RawMessage  `json:"reply_to_message"`
	Chat           *telegramChat    `json:"chat"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramUpdateShape struct {
	UpdateID          int64            `json:"update_id"`
	Message           *telegramMessage `json:"message"`
	EditedMessage     *telegramMessage `json:"edited_message"`
	ChannelPost       *telegramMessage `json:"channel_post"`
	EditedChannelPost *telegramMessage `json:"edited_channel_post"`
}

// NormalizeUpdate разбирает raw update. Ошибка — только неразборчивый
// update (не объект или без целочисленного update_id).
func NormalizeUpdate(raw json.RawMessage) (NormalizedEvent, error) {
	var shape telegramUpdateShape
	if err := json.Unmarshal(raw, &shape); err != nil {
		return NormalizedEvent{}, fmt.Errorf("нормализация: update не разобран: %w", err)
	}
	if shape.UpdateID == 0 {
		return NormalizedEvent{}, fmt.Errorf("нормализация: у update нет update_id")
	}

	message, edited := extractMessage(&shape)
	if message == nil || message.Chat == nil {
		// Неподдерживаемое: в журнал под фиктивным чатом 0, без обработки (§6.1).
		return NormalizedEvent{
			TelegramUpdateID: shape.UpdateID,
			ChatID:           0,
			ChatType:         ChatGroup,
			Kind:             KindOther,
			Supported:        false,
		}, nil
	}

	chatType := ChatGroup
	if message.Chat.Type == string(ChatPrivate) {
		chatType = ChatPrivate
	}
	return NormalizedEvent{
		TelegramUpdateID: shape.UpdateID,
		ChatID:           message.Chat.ID,
		ChatType:         chatType,
		ActorID:          actorIDOf(message),
		ActorName:        actorNameOf(message.From),
		Kind:             detectKind(message, edited),
		Text:             message.Text,
		Supported:        true,
	}, nil
}

func extractMessage(shape *telegramUpdateShape) (*telegramMessage, bool) {
	switch {
	case shape.Message != nil:
		return shape.Message, false
	case shape.EditedMessage != nil:
		return shape.EditedMessage, true
	case shape.ChannelPost != nil:
		return shape.ChannelPost, false
	case shape.EditedChannelPost != nil:
		return shape.EditedChannelPost, true
	default:
		return nil, false
	}
}

// detectKind — порядок проверки как в TS-справочнике: правка, reply,
// bot_command, mention, иначе сообщение.
func detectKind(message *telegramMessage, edited bool) EventKind {
	if edited {
		return KindEdited
	}
	if len(message.ReplyToMessage) > 0 && string(message.ReplyToMessage) != "null" {
		return KindReply
	}
	for _, entity := range message.Entities {
		switch entity.Type {
		case "bot_command":
			return KindCommand
		case "mention":
			return KindMention
		}
	}
	return KindMessage
}

func actorIDOf(message *telegramMessage) *int64 {
	if message.From == nil {
		return nil
	}
	id := message.From.ID
	return &id
}

// actorNameOf — first_name [last_name], иначе username (паритет TS).
func actorNameOf(from *telegramUser) *string {
	if from == nil {
		return nil
	}
	name := ""
	switch {
	case from.FirstName != "":
		name = from.FirstName
		if from.LastName != "" {
			name += " " + from.LastName
		}
	case from.Username != "":
		name = from.Username
	default:
		return nil
	}
	return &name
}
