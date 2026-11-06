// Заблокировавшие бота чаты (этап 38.7). Живой прогон 2026-11-06: доставка
// фонового check_in получила 403 «Forbidden: bot was blocked by the user», но
// чат продолжал жить в очереди — намерения и инициативы в него создавались
// дальше и падали. Пометка живёт в PostgreSQL (философия №2): 403 может
// прийти из доставки в любом процессе, а гейты фона и применения хода видят
// её запросом. Снимается любым новым входящим сообщением этого чата (ingest):
// пользователь разблокировал бота и написал сам.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// MarkChatBlocked помечает чат заблокированным и отменяет его ожидающие
// намерения одной командой (data-modifying CTE — одна транзакция по
// умолчанию): иначе check_in/share продолжат всплывать каждый такт и падать.
// Повторная пометка идемпотентна. Возвращает число отменённых намерений.
func MarkChatBlocked(ctx context.Context, db DB, chatID int64, reason string) (int64, error) {
	var cancelled int64
	err := db.QueryRow(ctx, `
		WITH blocked AS (
			INSERT INTO blocked_chats (chat_id, reason)
			VALUES ($1, $2)
			ON CONFLICT (chat_id) DO UPDATE
			  SET reason = EXCLUDED.reason, updated_at = now()
			RETURNING chat_id
		), dropped AS (
			UPDATE intentions SET status = 'cancelled', updated_at = now()
			WHERE target_chat_id = $1 AND status = 'pending'
			RETURNING id
		)
		SELECT count(*) FROM dropped`, chatID, reason).Scan(&cancelled)
	if err != nil {
		return 0, fmt.Errorf("заблокированный чат %d не помечен: %w", chatID, err)
	}
	return cancelled, nil
}

// ChatBlocked — чат помечен заблокированным (гейты фона/применения хода).
func ChatBlocked(ctx context.Context, db DB, chatID int64) (bool, error) {
	var blocked bool
	err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM blocked_chats WHERE chat_id = $1)`, chatID).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("пометка чата %d не читается: %w", chatID, err)
	}
	return blocked, nil
}

// UnblockChat снимает пометку (новое входящее сообщение чата).
func UnblockChat(ctx context.Context, db DB, chatID int64) error {
	if _, err := db.Exec(ctx, `DELETE FROM blocked_chats WHERE chat_id = $1`, chatID); err != nil {
		return fmt.Errorf("пометка чата %d не снята: %w", chatID, err)
	}
	return nil
}

// CountBlockedChats — метрика /stats (§20, этап 38.7).
func CountBlockedChats(ctx context.Context, db DB) (int64, error) {
	var n int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM blocked_chats`).Scan(&n); err != nil {
		return 0, fmt.Errorf("счётчик заблокированных чатов не читается: %w", err)
	}
	return n, nil
}

// ChatBlockedError — 403, которым Telegram сообщает, что писать больше некуда:
// пользователь заблокировал бота, удалил аккаунт или чат не существует.
// Только личка: в группе 403 значит «бота выгнали/нет прав» — это не повод
// отменять личные намерения.
func ChatBlockedError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.HTTPStatus != 403 && apiErr.HTTPStatus != 400 {
		return false
	}
	msg := strings.ToLower(apiErr.Description)
	for _, marker := range []string{
		"bot was blocked by the user",
		"user is deactivated",
		"chat not found",
		"bot was kicked",
		"peer_id_invalid",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
