// Whitelist лички (TELEGRAM_PRIVATE_ALLOWLIST, этап 42): с ботом в личке
// общаются только пользователи из списка, без участия модели. Группы не
// фильтруются. Пустой список — личка открыта всем (прежнее поведение).
//
// Вход: сообщение чужого сохраняется journal_only (offset не застревает, ход
// не создаётся) и не снимает пометку blocked_chats.
// Выход: GuardAPI не пускает ни один исходящий вызов в чужую личку (403 —
// delivery помечает чат заблокированным), а BlockForeignPrivateChats на
// старте помечает уже известные чужие лички — фон не планирует туда
// инициатив.
package telegram

import (
	"context"
	"fmt"
)

// allowlistDenied — описание отказа GuardAPI; ChatBlockedError его узнаёт.
const allowlistDenied = "Forbidden: private chat is not in allowlist"

// PrivateAllowlist — множество user id, которым разрешена личка; nil/пусто —
// фильтр выключен.
type PrivateAllowlist map[int64]struct{}

// NewPrivateAllowlist строит whitelist из списка id; пустой список — nil.
func NewPrivateAllowlist(ids []int64) PrivateAllowlist {
	if len(ids) == 0 {
		return nil
	}
	allow := make(PrivateAllowlist, len(ids))
	for _, id := range ids {
		allow[id] = struct{}{}
	}
	return allow
}

// Apply понижает статус события лички чужого до journal_only. В личке
// chat_id совпадает с user id собеседника.
func (a PrivateAllowlist) Apply(event NormalizedEvent, status IngestStatus) IngestStatus {
	if event.ChatType != ChatPrivate || a.allows(event.ChatID) {
		return status
	}
	return StatusJournalOnly
}

// allows — чат разрешён: фильтр выключен, чат не личка (в Telegram id лички
// положителен и равен user id, у групп и каналов — отрицателен) или id в
// списке.
func (a PrivateAllowlist) allows(chatID int64) bool {
	if len(a) == 0 || chatID < 0 {
		return true
	}
	_, ok := a[chatID]
	return ok
}

// GuardAPI оборачивает Bot API: исходящие вызовы в чужую личку не уходят в
// Telegram и возвращают постоянный 403. Пустой список — api без обёртки.
func GuardAPI(api API, allow PrivateAllowlist) API {
	if len(allow) == 0 {
		return api
	}
	return &guardedAPI{API: api, allow: allow}
}

type guardedAPI struct {
	API
	allow PrivateAllowlist
}

func (g *guardedAPI) deny(method string, chatID int64) error {
	if g.allow.allows(chatID) {
		return nil
	}
	return &APIError{Method: method, HTTPStatus: 403, Description: allowlistDenied}
}

func (g *guardedAPI) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (int64, error) {
	if err := g.deny("sendMessage", chatID); err != nil {
		return 0, err
	}
	return g.API.SendMessage(ctx, chatID, text, replyTo)
}

func (g *guardedAPI) SendSticker(ctx context.Context, chatID int64, fileID string) (int64, error) {
	if err := g.deny("sendSticker", chatID); err != nil {
		return 0, err
	}
	return g.API.SendSticker(ctx, chatID, fileID)
}

func (g *guardedAPI) SendPhoto(ctx context.Context, chatID int64, filename, contentType string, data []byte) (int64, error) {
	if err := g.deny("sendPhoto", chatID); err != nil {
		return 0, err
	}
	return g.API.SendPhoto(ctx, chatID, filename, contentType, data)
}

func (g *guardedAPI) ForwardMessage(ctx context.Context, chatID int64, fromChatID int64, messageID int64) (int64, error) {
	if err := g.deny("forwardMessage", chatID); err != nil {
		return 0, err
	}
	return g.API.ForwardMessage(ctx, chatID, fromChatID, messageID)
}

func (g *guardedAPI) SendChatAction(ctx context.Context, chatID int64, action string) error {
	if err := g.deny("sendChatAction", chatID); err != nil {
		return err
	}
	return g.API.SendChatAction(ctx, chatID, action)
}

func (g *guardedAPI) SetMessageReaction(ctx context.Context, chatID int64, messageID int64, emoji string) error {
	if err := g.deny("setMessageReaction", chatID); err != nil {
		return err
	}
	return g.API.SetMessageReaction(ctx, chatID, messageID, emoji)
}

// UnblockAllowedPrivateChats снимает пометки, поставленные whitelist'ом, с id,
// вернувшихся в список (M2 ревью 2026-11-07). Доставка пишет reason полным
// текстом ошибки («sendMessage: HTTP 403 — Forbidden: private chat is not in
// allowlist»), поэтому совпадение — по хвосту allowlistDenied. Настоящие
// блокировки Telegram (другой reason, например «bot was blocked by the user»)
// не трогаются: если пользователь действительно заблокировал бота, следующая
// доставка снова получит 403 и пометит чат заново.
func UnblockAllowedPrivateChats(ctx context.Context, db DB, allow PrivateAllowlist) (int, error) {
	if len(allow) == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(allow))
	for id := range allow {
		ids = append(ids, id)
	}
	tag, err := db.Exec(ctx, `
		DELETE FROM blocked_chats
		WHERE chat_id = ANY($1::bigint[])
		  AND (reason = $2 OR reason LIKE '%' || $2)`, ids, allowlistDenied)
	if err != nil {
		return 0, fmt.Errorf("пометки whitelist не сняты: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// BlockForeignPrivateChats помечает заблокированными все известные лички вне
// списка (с отменой их ожидающих намерений) — фон и применение хода не
// создают туда инициатив. Возвращает число помеченных чатов.
func BlockForeignPrivateChats(ctx context.Context, db DB, allow PrivateAllowlist) (int, error) {
	if len(allow) == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(allow))
	for id := range allow {
		ids = append(ids, id)
	}
	rows, err := db.Query(ctx, `
		SELECT DISTINCT chat_id FROM social_events
		WHERE chat_type = 'private' AND chat_id <> ALL($1::bigint[])
		  AND NOT EXISTS (SELECT 1 FROM blocked_chats b WHERE b.chat_id = social_events.chat_id)`, ids)
	if err != nil {
		return 0, fmt.Errorf("чужие лички не читаются: %w", err)
	}
	var chats []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("чужая личка не читается: %w", err)
		}
		chats = append(chats, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("чужие лички не читаются: %w", err)
	}
	for _, id := range chats {
		if _, err := MarkChatBlocked(ctx, db, id, allowlistDenied); err != nil {
			return 0, err
		}
	}
	return len(chats), nil
}
