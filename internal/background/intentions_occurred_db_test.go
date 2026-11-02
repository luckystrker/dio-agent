//go:build db

// Независимая смысловая проверка 18.1 на потребителе occurred_at: гейт
// «чат ушёл вперёд» (§14.1, IntentionRepo.ChatMoved) обязан считать по времени
// СООБЩЕНИЯ, а не приёма. Сообщение, написанное до создания намерения, но
// принятое после простоя (накопленная пачка), не должно гасить инициативу как
// «неуместную»: при occurred_at = времени приёма (дефект) гасилось.
package background

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

func TestChatMovedUsesMessageTimeNotIngestTime(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const chat = int64(-300123)
	repo := NewIntentionRepo(pool)

	// Намерение создано «сейчас»; сообщение написано 2 часа назад, принято
	// только сейчас (простой → накопленная пачка пришла поздно).
	intentionAt := time.Now().UTC().Truncate(time.Second)
	sentAt := intentionAt.Add(-2 * time.Hour)

	old := fmt.Sprintf(`{"update_id":9001,"message":{"message_id":1,"date":%d,`+
		`"text":"давно написанное","from":{"id":5,"first_name":"Коля"},`+
		`"chat":{"id":%d,"type":"group"}}}`, sentAt.Unix(), chat)
	if _, err := telegram.IngestUpdate(ctx, pool, []byte(old)); err != nil {
		t.Fatalf("ingest старого сообщения: %v", err)
	}

	moved, err := repo.ChatMoved(ctx, chat, intentionAt)
	if err != nil {
		t.Fatalf("ChatMoved: %v", err)
	}
	if moved {
		t.Fatalf("сообщение, отправленное ДО создания намерения, посчитано «чатом ушёл вперёд» — occurred_at равен времени приёма, а не сообщения (18.1)")
	}

	// Контроль направления проверки: сообщение, написанное после создания
	// намерения (на час позже — вне гранулярности секунд), замечается гейтом.
	fresh := fmt.Sprintf(`{"update_id":9002,"message":{"message_id":2,"date":%d,`+
		`"text":"свежее","from":{"id":5,"first_name":"Коля"},`+
		`"chat":{"id":%d,"type":"group"}}}`, intentionAt.Add(time.Hour).Unix(), chat)
	if _, err := telegram.IngestUpdate(ctx, pool, []byte(fresh)); err != nil {
		t.Fatalf("ingest свежего сообщения: %v", err)
	}
	moved, err = repo.ChatMoved(ctx, chat, intentionAt)
	if err != nil {
		t.Fatalf("ChatMoved: %v", err)
	}
	if !moved {
		t.Fatalf("свежее сообщение не замечено гейтом «чат ушёл вперёд» — контроль направления проверки не работает")
	}
}
