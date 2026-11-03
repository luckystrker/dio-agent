//go:build db

// БД-тесты правок (30.4): событие «было → стало», актуальная версия в
// исходной строке истории, повтор update не откатывает текст.
package telegram

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func rawEditOf(updateID, messageID, chatID int64, text string) []byte {
	return []byte(fmt.Sprintf(`{"update_id":%d,"edited_message":{"message_id":%d,"from":{"id":7,"first_name":"Коля"},"chat":{"id":%d,"type":"private"},"date":%d,"edit_date":%d,"text":%q}}`,
		updateID, messageID, chatID, time.Now().Unix(), time.Now().Unix(), text))
}

func TestIngestEditComposesAndUpdatesHistory(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const chatID = 501

	original := mustIngest(t, pool, rawMessageOf(9100, chatID, "привет"))
	if original.Status != StatusPending {
		t.Fatalf("исходное сообщение: status=%s", original.Status)
	}
	edit := mustIngest(t, pool, rawEditOf(9101, 9100, chatID, "привет, это я"))
	if edit.Event.Kind != KindEdited || edit.Status != StatusPending {
		t.Fatalf("правка: kind=%s status=%s", edit.Event.Kind, edit.Status)
	}

	// История показывает актуальную версию исходной строки, без дубля.
	var originalText string
	if err := pool.QueryRow(ctx,
		`SELECT text FROM social_events WHERE id = $1`, original.EventID).Scan(&originalText); err != nil {
		t.Fatalf("исходная строка не читается: %v", err)
	}
	if originalText != "привет, это я" {
		t.Fatalf("история не обновлена: %q", originalText)
	}
	var editText string
	var version int
	var messageID int64
	if err := pool.QueryRow(ctx,
		`SELECT text, version, telegram_message_id FROM social_events WHERE id = $1`, edit.EventID).Scan(&editText, &version, &messageID); err != nil {
		t.Fatalf("правка не читается: %v", err)
	}
	if version != 1 || messageID != 9100 {
		t.Fatalf("правка: version=%d message_id=%d", version, messageID)
	}
	if !strings.Contains(editText, "поправил сообщение") ||
		!strings.Contains(editText, "«привет»") || !strings.Contains(editText, "«привет, это я»") {
		t.Fatalf("текст события правки: %q", editText)
	}

	// Повтор того же update — не ошибка и не откат текста.
	if dup := mustIngest(t, pool, rawEditOf(9101, 9100, chatID, "привет, это я")); dup.Inserted {
		t.Fatalf("повтор правки записан второй раз")
	}

	// Вторая правка: «было» — актуальная версия после первой.
	second := mustIngest(t, pool, rawEditOf(9102, 9100, chatID, "привет"))
	var secondText string
	var secondVersion int
	if err := pool.QueryRow(ctx,
		`SELECT text, version FROM social_events WHERE id = $1`, second.EventID).Scan(&secondText, &secondVersion); err != nil {
		t.Fatalf("вторая правка не читается: %v", err)
	}
	if secondVersion != 2 {
		t.Fatalf("версия второй правки=%d", secondVersion)
	}
	if !strings.Contains(secondText, "«привет, это я»") || !strings.Contains(secondText, "«привет»") {
		t.Fatalf("текст второй правки: %q", secondText)
	}
	if err := pool.QueryRow(ctx,
		`SELECT text FROM social_events WHERE id = $1`, original.EventID).Scan(&originalText); err != nil {
		t.Fatalf("исходная строка не читается: %v", err)
	}
	if originalText != "привет" {
		t.Fatalf("актуальная версия после второй правки: %q", originalText)
	}

	// Событие правки не должно попасть в недавнюю историю как реплика:
	// kind=edited исключён выборкой истории (проверка в character пакете).
	var editedRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE chat_id = $1 AND kind = 'edited'`, chatID).Scan(&editedRows); err != nil || editedRows != 2 {
		t.Fatalf("строк правок=%d err=%v, хочу 2", editedRows, err)
	}
}

func TestEditedTextLimitAndMissingOriginal(t *testing.T) {
	long := strings.Repeat("а", editTextLimit+50)
	text := editedText(nil, &long)
	if !strings.HasPrefix(text, "[поправил сообщение: «") {
		t.Fatalf("текст без «было»: %q", text)
	}
	if len([]rune(text)) > editTextLimit+40 {
		t.Fatalf("потолок длины правки не работает: %d рун", len([]rune(text)))
	}
}
