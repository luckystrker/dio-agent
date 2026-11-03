// Правки сообщений (этап 30.4): edited_message — не новое сообщение, а
// событие «пользователь поправил сообщение: «было» → «стало»» в ход. История
// чата показывает АКТУАЛЬНУЮ версию исходного сообщения (текст строки
// обновляется), а не дубль: событие kind=edited в недавнюю историю не входит.
//
// «Было» берётся из social_events по (chat_id, telegram_message_id); исходного
// сообщения в журнале нет (бот добавлен позже) — событие честно говорит только
// про новое. Обе стороны обрезаются потолком: правка не раздувает ход.
package telegram

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// editTextLimit — потолок «было»/«стало» в тексте события правки (руны).
const editTextLimit = 300

// editedText — текст события правки: «было» → «стало» с потолком длины.
// Нет исходного текста — только новое; нет нового — честная пометка.
func editedText(before, after *string) string {
	old := trimEditText(before)
	now := trimEditText(after)
	switch {
	case old != "" && now != "":
		return "[поправил сообщение: «" + old + "» → «" + now + "»]"
	case now != "":
		return "[поправил сообщение: «" + now + "»]"
	case old != "":
		return "[поправил сообщение: «" + old + "» → (без текста)]"
	default:
		return "[поправил сообщение]"
	}
}

// trimEditText — однострочный текст с потолком: правка цитатой не ломает
// строку события.
func trimEditText(text *string) string {
	if text == nil {
		return ""
	}
	value := strings.Join(strings.Fields(*text), " ")
	if runes := []rune(value); len(runes) > editTextLimit {
		value = string(runes[:editTextLimit-1]) + "…"
	}
	return value
}

// applyEditedMessage дописывает событие правки в журнал: находит исходную
// строку по telegram_message_id, обновляет её текст (актуальная версия в
// истории) и возвращает текст для события правки и номер версии.
func applyEditedMessage(ctx context.Context, tx pgx.Tx, event NormalizedEvent) (text *string, version int, err error) {
	version = 0
	if event.MessageID == 0 {
		// Старые/неполные update'ы без message_id: исходное не найти, событие
		// остаётся текстом правки без «было» (прежняя деградация, не ошибка).
		return event.Text, version, nil
	}

	var originalID int64
	var before *string
	err = tx.QueryRow(ctx, `
		SELECT id, text FROM social_events
		WHERE chat_id = $1 AND telegram_message_id = $2 AND kind <> 'edited'
		ORDER BY version, id LIMIT 1`, event.ChatID, event.MessageID).Scan(&originalID, &before)
	if err != nil && err != pgx.ErrNoRows {
		return nil, 0, fmt.Errorf("ingest: исходное сообщение правки не читается: %w", err)
	}
	if err == nil {
		if _, uerr := tx.Exec(ctx,
			`UPDATE social_events SET text = $2 WHERE id = $1`, originalID, event.Text); uerr != nil {
			return nil, 0, fmt.Errorf("ingest: текст исходного сообщения не обновлён: %w", uerr)
		}
	}
	if verr := tx.QueryRow(ctx, `
		SELECT coalesce(max(version), 0) + 1 FROM social_events
		WHERE chat_id = $1 AND telegram_message_id = $2 AND kind = 'edited'`,
		event.ChatID, event.MessageID).Scan(&version); verr != nil {
		return nil, 0, fmt.Errorf("ingest: версия правки не посчитана: %w", verr)
	}
	composed := editedText(before, event.Text)
	return &composed, version, nil
}

// editVersionForInsert — версия строки при вставке: у обычного сообщения 0,
// у правки — её номер (0 = исходное сообщение, N = номер правки, §18).
func editVersionForInsert(kind EventKind, version int) int {
	if kind != KindEdited {
		return 0
	}
	return version
}
