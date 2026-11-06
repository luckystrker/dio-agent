//go:build db

// БД-тесты пометки «чат заблокировал бота» (этап 38.7): 403 доставки ставит
// пометку и отменяет ожидающие намерения чата, новое входящее сообщение
// снимает пометку, счётчик виден метрике.
package telegram

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// blockedChatState — пометка чата и статус намерения (intentionID 0 — не читать).
func blockedChatState(t *testing.T, pool *runtime.Pool, chatID, intentionID int64) (blocked bool, status string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM blocked_chats WHERE chat_id = $1)`, chatID).Scan(&blocked); err != nil {
		t.Fatalf("пометка не читается: %v", err)
	}
	if intentionID != 0 {
		if err := pool.QueryRow(ctx, `SELECT status FROM intentions WHERE id = $1`, intentionID).Scan(&status); err != nil {
			t.Fatalf("статус намерения не читается: %v", err)
		}
	}
	return blocked, status
}

func TestDelivery403MarksChatBlockedAndCancelsIntentions(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const chatID = int64(-548919987)

	// Ожидающее намерение чата: после 403 оно не должно висеть в очереди.
	var provenanceID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO provenance (kind, ref, scopes) VALUES ('runtime_action', 'test:blocked', ARRAY['chat:-548919987']) RETURNING id`).
		Scan(&provenanceID); err != nil {
		t.Fatalf("provenance не создан: %v", err)
	}
	var intentionID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO intentions (kind, target_chat_id, topic, reason, provenance_id, dedupe_key)
		VALUES ('check_in', $1, 'как дела', 'тест 38.7', $2, 'test-blocked') RETURNING id`, chatID, provenanceID).
		Scan(&intentionID); err != nil {
		t.Fatalf("намерение не создано: %v", err)
	}

	stub := &telegramStub{sendMessage: func(map[string]any) (int, string) {
		return http.StatusForbidden, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`
	}}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)
	id := insertOutbox(t, pool, 232, chatID, 0, `{"kind":"message","text":"привет"}`)

	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{BubbleGap: time.Millisecond})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	// Доставка по-прежнему терминально failed (§13.2: 403 — постоянная ошибка).
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM delivery_outbox WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("статус строки не читается: %v", err)
	}
	if status != "failed" {
		t.Fatalf("статус строки = %q, хочу failed", status)
	}

	blocked, intentionStatus := blockedChatState(t, pool, chatID, intentionID)
	if !blocked {
		t.Fatal("чат не помечен заблокированным после 403 bot was blocked by the user")
	}
	if intentionStatus != "cancelled" {
		t.Fatalf("ожидающее намерение осталось %q, хочу cancelled", intentionStatus)
	}

	// Повторная пометка идемпотентна.
	if _, err := MarkChatBlocked(ctx, pool, chatID, "повтор"); err != nil {
		t.Fatalf("повторная пометка упала: %v", err)
	}
	if n, err := CountBlockedChats(ctx, pool); err != nil || n != 1 {
		t.Fatalf("счётчик заблокированных = %d (err=%v), хочу 1", n, err)
	}
}

func TestIngestUnblocksChat(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	mustIngest(t, pool, rawMessageOf(300, 42, "привет"))
	if _, err := MarkChatBlocked(ctx, pool, 42, "Forbidden: bot was blocked by the user"); err != nil {
		t.Fatalf("чат не помечен: %v", err)
	}
	if blocked, err := ChatBlocked(ctx, pool, 42); err != nil || !blocked {
		t.Fatalf("пометка не читается: blocked=%v err=%v", blocked, err)
	}

	// Пользователь разблокировал бота и написал — пометка снимается ingest'ом.
	mustIngest(t, pool, rawMessageOf(301, 42, "я снова тут"))
	if blocked, err := ChatBlocked(ctx, pool, 42); err != nil {
		t.Fatalf("пометка не читается: %v", err)
	} else if blocked {
		t.Fatal("новое входящее сообщение не сняло пометку заблокированного чата")
	}
}
