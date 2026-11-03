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
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// ErrUnusableUpdate — update неисправим на уровне данных (не разобран или
// без update_id): повтор не поможет, poller скипает его с продвижением
// offset, чтобы один такой update не глушил приём (16.4).
var ErrUnusableUpdate = errors.New("update неисправим")

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
	KindPhoto   EventKind = "photo"
	KindReply   EventKind = "reply"
	KindSticker EventKind = "sticker"
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
	// Date — время сообщения из update (секунды Unix, message.date); 0 — даты
	// нет, occurred_at возьмёт now() приёма (18.1).
	Date int64
	// Supported=false — update не поддерживается: журнал без обработки.
	Supported bool
	// BotKnown — идентичность бота известна (getMe удался): только тогда
	// ReplyToBot/MentionBot имеют смысл.
	BotKnown   bool
	ReplyToBot bool
	MentionBot bool
}

// telegramEntity, telegramMessage, telegramUpdateShape — узкое представление
// нужных полей raw update; всё остальное остаётся в raw_update (jsonb).
type telegramEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type telegramMessage struct {
	From            *telegramUser       `json:"from"`
	Date            int64               `json:"date"` // секунды Unix (время сообщения, не приёма)
	Text            *string             `json:"text"`
	Caption         *string             `json:"caption"`
	Photo           []telegramPhotoSize `json:"photo"`
	Sticker         *telegramSticker    `json:"sticker"`
	Animation       *telegramAnimation  `json:"animation"`
	Voice           *telegramVoice      `json:"voice"`
	VideoNote       *telegramVideoNote  `json:"video_note"`
	Entities        []telegramEntity    `json:"entities"`
	CaptionEntities []telegramEntity    `json:"caption_entities"`
	ReplyToMessage  *telegramMessage    `json:"reply_to_message"`
	Chat            *telegramChat       `json:"chat"`
}

// telegramSticker — стикер (этап 25): эмодзи становится текстом события,
// file_id — картинкой в запрос модели (статичный) или источником для
// коллекции/отправки; анимированный/видео отдаются превью.
type telegramSticker struct {
	FileID       string         `json:"file_id"`
	FileUniqueID string         `json:"file_unique_id"`
	Emoji        string         `json:"emoji"`
	SetName      string         `json:"set_name"`
	IsVideo      bool           `json:"is_video"`
	IsAnimated   bool           `json:"is_animated"`
	Thumbnail    *telegramThumb `json:"thumbnail"`
	Thumb        *telegramThumb `json:"thumb"` // устаревшее имя поля (Bot API < 6.6)
}

// previewFileID — картинка стикера для модели: своё превью (thumbnail, затем
// устаревшее thumb); пусто — превью нет.
func (s *telegramSticker) previewFileID() string {
	if s == nil {
		return ""
	}
	if s.Thumbnail != nil && s.Thumbnail.FileID != "" {
		return s.Thumbnail.FileID
	}
	if s.Thumb != nil {
		return s.Thumb.FileID
	}
	return ""
}

// telegramAnimation — гифка: собственный файл (.mp4) картинкой не отдаётся,
// модели идёт кадр-превью (этап 25.2).
type telegramAnimation struct {
	FileID       string         `json:"file_id"`
	FileUniqueID string         `json:"file_unique_id"`
	Duration     int            `json:"duration"`
	Thumbnail    *telegramThumb `json:"thumbnail"`
	Thumb        *telegramThumb `json:"thumb"`
}

// previewFileID — file_id кадра-превью гифки; пусто — превью нет.
func (a *telegramAnimation) previewFileID() string {
	if a == nil {
		return ""
	}
	if a.Thumbnail != nil && a.Thumbnail.FileID != "" {
		return a.Thumbnail.FileID
	}
	if a.Thumb != nil {
		return a.Thumb.FileID
	}
	return ""
}

// telegramThumb — превью вложения (thumbnail/thumb).
type telegramThumb struct {
	FileID string `json:"file_id"`
}

// telegramVoice / telegramVideoNote — голосовое и «кружок»: до модели доходит
// только длительность (25.6), транскрипции нет (бэклог).
type telegramVoice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

type telegramVideoNote struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

// telegramPhotoSize — один размер из массива photo (Telegram отдаёт по возрастанию).
type telegramPhotoSize struct {
	FileID string `json:"file_id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
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
	return NormalizeUpdateWithBot(raw, BotIdentity{})
}

// NormalizeUpdateWithBot — NormalizeUpdate с идентичностью бота: reply
// засчитывается только на сообщение бота, mention — только на его username
// (M8). Без идентичности флаги остаются ложными, prefilter работает по
// прежним правилам (dry-run и тесты без getMe).
func NormalizeUpdateWithBot(raw json.RawMessage, bot BotIdentity) (NormalizedEvent, error) {
	var shape telegramUpdateShape
	if err := json.Unmarshal(raw, &shape); err != nil {
		return NormalizedEvent{}, fmt.Errorf("%w: update не разобран: %v", ErrUnusableUpdate, err)
	}
	if shape.UpdateID == 0 {
		return NormalizedEvent{}, fmt.Errorf("%w: у update нет update_id", ErrUnusableUpdate)
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
	known := bot.ID != 0 || bot.Username != ""
	return NormalizedEvent{
		TelegramUpdateID: shape.UpdateID,
		ChatID:           message.Chat.ID,
		ChatType:         chatType,
		ActorID:          actorIDOf(message),
		ActorName:        actorNameOf(message.From),
		Kind:             detectKind(message, edited),
		Text:             eventText(message),
		Date:             message.Date,
		Supported:        true,
		BotKnown:         known,
		ReplyToBot:       known && isReplyToBot(message, bot),
		MentionBot:       known && hasBotMention(message, bot),
	}, nil
}

// isReplyToBot — reply именно на сообщение этого бота (не на любое в чате).
func isReplyToBot(message *telegramMessage, bot BotIdentity) bool {
	if bot.ID == 0 || message.ReplyToMessage == nil || message.ReplyToMessage.From == nil {
		return false
	}
	return message.ReplyToMessage.From.ID == bot.ID
}

// hasBotMention — в сообщении есть entity mention, и её диапазон (offset/
// length в единицах UTF-16) указывает ровно на @username бота. Сравнение
// регистронезависимое, как у Telegram. Подстрока не подходит: @dio_fan не
// должен считаться обращением к @dio (minor ревью).
func hasBotMention(message *telegramMessage, bot BotIdentity) bool {
	if bot.Username == "" {
		return false
	}
	text := messageText(message)
	if text == nil {
		return false
	}
	entities := message.Entities
	if message.Text == nil && message.Caption != nil {
		entities = message.CaptionEntities
	}
	want := "@" + bot.Username
	units := utf16.Encode([]rune(*text))
	for _, entity := range entities {
		if entity.Type != "mention" {
			continue
		}
		if entity.Offset < 0 || entity.Length <= 0 || entity.Offset+entity.Length > len(units) {
			continue
		}
		frag := string(utf16.Decode(units[entity.Offset : entity.Offset+entity.Length]))
		if strings.EqualFold(frag, want) {
			return true
		}
	}
	return false
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

// messageText — текст сообщения; для фото и гифки это caption (если есть).
func messageText(m *telegramMessage) *string {
	if m.Text != nil {
		return m.Text
	}
	if (len(m.Photo) > 0 || m.Animation != nil || m.Sticker != nil) && m.Caption != nil {
		return m.Caption
	}
	return nil
}

// replyQuoteLimit — потолок цитаты реплая в рунах: достаточно для смысла,
// не раздувает ход и историю.
const replyQuoteLimit = 200

// eventText — текст события для хода и истории: собственный текст/подпись;
// у стикера — эмодзи, у гифки — «[гифка]» с подписью, у голосового/кружка —
// вид и длительность. У реплая — цитата сообщения, на которое ответили
// (находка приёмки Р: без неё модель слепа, когда исходное вне окна недавнего
// разговора). nil — текста нет.
func eventText(m *telegramMessage) *string {
	base := ""
	switch {
	case m.Sticker != nil:
		if m.Sticker.Emoji != "" {
			base = "[стикер " + m.Sticker.Emoji + "]"
		} else {
			// Стикер без эмодзи не должен превращаться в «сообщение без
			// текста» в пачке хода (замечание приёмки 13).
			base = "[стикер]"
		}
	case m.Animation != nil:
		// Гифка: модели идёт кадр-превью (media_stickers.go), а в тексте
		// события — вид вложения и подпись, чтобы смысл не терялся.
		base = "[гифка]"
		if caption := strings.TrimSpace(deref(m.Caption)); caption != "" {
			base += " " + caption
		}
	case m.Voice != nil:
		base = "[голосовое, " + formatMediaDuration(m.Voice.Duration) + "]"
	case m.VideoNote != nil:
		base = "[кружок, " + formatMediaDuration(m.VideoNote.Duration) + "]"
	default:
		if t := messageText(m); t != nil {
			base = *t
		}
	}
	if m.ReplyToMessage != nil {
		base = replyPrefix(m.ReplyToMessage) + base
	}
	if base == "" {
		return nil
	}
	return &base
}

// formatMediaDuration — длительность вложения «0:42», «2:05», «1:02:03»:
// короткий и однозначный вид для текста события и истории.
func formatMediaDuration(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	h, m, s := seconds/3600, (seconds%3600)/60, seconds%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// deref — значение строки-указателя; nil — пусто.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// replyPrefix — метка-цитата «в ответ на …»: имя автора и потолок текста.
func replyPrefix(quoted *telegramMessage) string {
	inner := quotedContent(quoted)
	if name := actorNameOf(quoted.From); name != nil {
		return "[в ответ на " + *name + ": " + inner + "] "
	}
	return "[в ответ на " + inner + "] "
}

// quotedContent — что именно цитируется: текст (обрезанный), фото, стикер
// или честное «без текста».
func quotedContent(quoted *telegramMessage) string {
	if t := messageText(quoted); t != nil && strings.TrimSpace(*t) != "" {
		runes := []rune(strings.TrimSpace(*t))
		if len(runes) > replyQuoteLimit {
			return "«" + string(runes[:replyQuoteLimit]) + "…»"
		}
		return "«" + string(runes) + "»"
	}
	if len(quoted.Photo) > 0 {
		return "(фото)"
	}
	if quoted.Sticker != nil {
		return "(стикер)"
	}
	return "(без текста)"
}

// detectKind — порядок проверки: правка, reply, стикер, фото, bot_command,
// mention, иначе сообщение.
func detectKind(message *telegramMessage, edited bool) EventKind {
	if edited {
		return KindEdited
	}
	if message.ReplyToMessage != nil {
		return KindReply
	}
	if message.Sticker != nil {
		return KindSticker
	}
	if len(message.Photo) > 0 {
		return KindPhoto
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
