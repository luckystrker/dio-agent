//go:build db

// БД-тесты delivery loop §13 против Bot API-заглушки (httptest): подтверждённый
// успех, 429 с honor retry_after, таймаут → unknown + отмена зависимых,
// устаревание перед отправкой §13.3, реакция, постоянная ошибка.
package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// telegramStub — заглушка Bot API с записью вызовов sendMessage.
type telegramStub struct {
	mu        sync.Mutex
	messages  []map[string]any
	reactions []map[string]any
	// sendMessage — поведение; nil — успешный ответ с порядковым message_id.
	sendMessage func(body map[string]any) (int, string) // (status, body)
}

func (s *telegramStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("content-type", "application/json")
		switch {
		case r.URL.Path == "/botTESTTOKEN/sendMessage":
			s.mu.Lock()
			s.messages = append(s.messages, body)
			s.mu.Unlock()
			if s.sendMessage != nil {
				status, payload := s.sendMessage(body)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(payload))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":777}}`))
		case r.URL.Path == "/botTESTTOKEN/setMessageReaction":
			s.mu.Lock()
			s.reactions = append(s.reactions, body)
			s.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *telegramStub) sentMessages() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any{}, s.messages...)
}

func TestDeliveryLoopSendsMessage(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)

	id := insertOutbox(t, pool, 100, 42, 0, `{"kind":"message","text":"привет от хода"}`)
	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{BubbleGap: time.Millisecond})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	var status string
	var telegramID *int64
	var lastError *string
	if err := pool.QueryRow(ctx,
		`SELECT status, telegram_message_id, last_error FROM delivery_outbox WHERE id = $1`, id).
		Scan(&status, &telegramID, &lastError); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "sent" || telegramID == nil || *telegramID != 777 || lastError != nil {
		t.Fatalf("доставка: status=%s id=%v err=%v", status, telegramID, lastError)
	}
	sent := stub.sentMessages()
	if len(sent) != 1 || sent[0]["text"] != "привет от хода" || sent[0]["chat_id"].(float64) != 42 {
		t.Fatalf("sendMessage не дошёл или битый: %v", sent)
	}
}

func TestDeliveryLoopSendsBubblesInOrder(t *testing.T) {
	// §13.1 порядок внутри чата + §13.3 пауза между bubbles.
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)

	insertOutbox(t, pool, 110, 42, 0, `{"kind":"message","text":"раз"}`)
	insertOutbox(t, pool, 110, 42, 1, `{"kind":"message","text":"два"}`)
	insertOutbox(t, pool, 110, 42, 2, `{"kind":"message","text":"три"}`)

	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{BubbleGap: 30 * time.Millisecond})
	start := time.Now()
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	sent := stub.sentMessages()
	if len(sent) != 3 {
		t.Fatalf("отправлено %d пузырей, хочу 3", len(sent))
	}
	for i, want := range []string{"раз", "два", "три"} {
		if sent[i]["text"] != want {
			t.Fatalf("пузырь %d = %v, хочу %q", i, sent[i]["text"], want)
		}
	}
	// Две паузы по 30 мс между тремя пузырями.
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("пауза между bubbles не выдержана: %v", elapsed)
	}
	var pending int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE status = 'sent'`).Scan(&pending); err != nil || pending != 3 {
		t.Fatalf("sent строк: %d (err=%v)", pending, err)
	}
}

func TestDeliveryLoop429RetryAfter(t *testing.T) {
	// §13.2: подтверждённый отказ без отправки, причина временная — retry_wait
	// с honor retry_after.
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{sendMessage: func(map[string]any) (int, string) {
		return http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"retry after 2","parameters":{"retry_after":2}}`
	}}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)

	id := insertOutbox(t, pool, 120, 42, 0, `{"kind":"message","text":"позже"}`)
	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	var status string
	var notBefore time.Time
	var lastError *string
	if err := pool.QueryRow(ctx,
		`SELECT status, not_before, last_error FROM delivery_outbox WHERE id = $1`, id).
		Scan(&status, &notBefore, &lastError); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "retry_wait" {
		t.Fatalf("статус = %s, хочу retry_wait", status)
	}
	if wait := time.Until(notBefore); wait < 1500*time.Millisecond {
		t.Fatalf("honor retry_after не выдержан: not_before через %v", wait)
	}
	if lastError == nil {
		t.Fatalf("причина отказа не записана")
	}
}

func TestDeliveryLoopTimeoutBecomesUnknownAndCancelsDependents(t *testing.T) {
	// §13.2: timeout — исход неизвестен; автоматически НЕ повторяется,
	// зависимые пузыри последовательности отменяются.
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{sendMessage: func(map[string]any) (int, string) {
		time.Sleep(300 * time.Millisecond) // дольше таймаута клиента
		return 200, `{"ok":true,"result":{"message_id":1}}`
	}}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	client.sendTimeout = 50 * time.Millisecond
	repo := NewOutboxRepo(pool)

	id0 := insertOutbox(t, pool, 130, 42, 0, `{"kind":"message","text":"неизвестно"}`)
	id1 := insertOutbox(t, pool, 130, 42, 1, `{"kind":"message","text":"зависим"}`)

	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	statuses := map[int64]string{}
	rows, err := pool.Query(ctx, `SELECT id, status FROM delivery_outbox`)
	if err != nil {
		t.Fatalf("статусы не читаются: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st string
		if err := rows.Scan(&id, &st); err != nil {
			t.Fatalf("статус не читается: %v", err)
		}
		statuses[id] = st
	}
	rows.Close()
	if statuses[id0] != "unknown" {
		t.Fatalf("таймаут должен давать unknown: %s", statuses[id0])
	}
	if statuses[id1] != "cancelled" {
		t.Fatalf("зависимый пузырь должен быть отменён: %s", statuses[id1])
	}
}

func TestDeliveryLoopStalenessCancelsBubbles(t *testing.T) {
	// §13.3: новые события чата после создания turn делают продолжение
	// неуместным — оставшиеся bubbles отменяются.
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)

	// Новое pending-событие чата 42 (turn_id NULL) — чат ушёл вперёд.
	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, kind, raw_update, status)
		VALUES (9001, 42, 'private', 'message', '{}', 'pending')`); err != nil {
		t.Fatalf("pending-событие не вставлено: %v", err)
	}
	insertOutbox(t, pool, 140, 42, 0, `{"kind":"message","text":"уже неуместно"}`)

	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	var status string
	var lastError *string
	if err := pool.QueryRow(ctx,
		`SELECT status, last_error FROM delivery_outbox WHERE turn_id = 140`).Scan(&status, &lastError); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "cancelled" || lastError == nil {
		t.Fatalf("устаревший пузырь должен быть отменён: %s %v", status, lastError)
	}
	if len(stub.sentMessages()) != 0 {
		t.Fatalf("устаревшее не должно было уйти в сеть")
	}
}

func TestDeliveryLoopReaction(t *testing.T) {
	// §13.2/§11.2: реакция — установка состояния; telegram_message_id — NULL.
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)

	id := insertOutbox(t, pool, 150, 42, 0, `{"kind":"reaction","reaction":"🔥","target_message_id":7}`)
	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	var status string
	var telegramID *int64
	if err := pool.QueryRow(ctx,
		`SELECT status, telegram_message_id FROM delivery_outbox WHERE id = $1`, id).Scan(&status, &telegramID); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "sent" || telegramID != nil {
		t.Fatalf("реакция: status=%s id=%v", status, telegramID)
	}
	if len(stub.reactions) != 1 || stub.reactions[0]["emoji"] != "🔥" || stub.reactions[0]["message_id"].(float64) != 7 {
		t.Fatalf("setMessageReaction не дошёл: %v", stub.reactions)
	}
}

func TestDeliveryLoopPermanentFailure(t *testing.T) {
	// §13.2: подтверждённая постоянная ошибка (4xx) — failed.
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{sendMessage: func(map[string]any) (int, string) {
		return http.StatusForbidden, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`
	}}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)

	id := insertOutbox(t, pool, 160, 42, 0, `{"kind":"message","text":"заблокирован"}`)
	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	var status string
	var lastError *string
	if err := pool.QueryRow(ctx,
		`SELECT status, last_error FROM delivery_outbox WHERE id = $1`, id).Scan(&status, &lastError); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "failed" || lastError == nil {
		t.Fatalf("4xx должен давать failed: %s %v", status, lastError)
	}
}

func TestDeliveryLoopRecoversFullTurnFromQueue(t *testing.T) {
	// Восстановление после сбоя: строки, пережившие процесс (pending),
	// отправляются заново — без дублей в БД (одна строка — одна отправка).
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)

	id := insertOutbox(t, pool, 170, 42, 0, `{"kind":"message","text":"переживает сбой"}`)

	// Первый «процесс» упал сразу после записи строки: новых тактов не было.
	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт 1: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM delivery_outbox WHERE id = $1`, id).Scan(&status); err != nil || status != "sent" {
		t.Fatalf("после такта 1: %s (err=%v)", status, err)
	}
	// Повторный такт (перезапуск) ничего не отправляет повторно.
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт 2: %v", err)
	}
	if sent := stub.sentMessages(); len(sent) != 1 {
		t.Fatalf("дубль после перезапуска: %d отправок", len(sent))
	}
}
