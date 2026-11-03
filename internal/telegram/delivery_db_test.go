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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luckystrker/dio-agent/internal/runtime"
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
	// §13.1 порядок внутри чата + §13.3 пауза между bubbles. После 17.7 клейм
	// берёт голову очереди чата: пузыри уходят по одному за такт, следующий —
	// после исхода предыдущего, порядок не может перевернуться.
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
	// Такты до полной отправки (голова очереди за такт); зависание — падение
	// по дедлайну, а не вечное ожидание.
	for len(stub.sentMessages()) < 3 {
		if time.Since(start) > 5*time.Second {
			t.Fatalf("отправлено %d пузырей из 3 — очередь встала", len(stub.sentMessages()))
		}
		if err := loop.Tick(ctx); err != nil {
			t.Fatalf("такт не удался: %v", err)
		}
	}

	sent := stub.sentMessages()
	for i, want := range []string{"раз", "два", "три"} {
		if sent[i]["text"] != want {
			t.Fatalf("пузырь %d = %v, хочу %q", i, sent[i]["text"], want)
		}
	}
	// Две паузы по 30 мс между тремя пузырями.
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("пауза между bubbles не выдержана: %v", elapsed)
	}
	var sentCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE status = 'sent'`).Scan(&sentCount); err != nil || sentCount != 3 {
		t.Fatalf("sent строк: %d (err=%v)", sentCount, err)
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
	// §13.3: новые события чата делают хвостовые bubbles неуместными —
	// отменяются; первый пузырь (позиция 0) уже ушёл как прямой ответ.
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
	insertOutboxSent(t, pool, 140, 42, 0, `{"kind":"message","text":"прямой ответ"}`)
	insertOutbox(t, pool, 140, 42, 1, `{"kind":"message","text":"уже неуместно"}`)

	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	var status string
	var lastError *string
	if err := pool.QueryRow(ctx,
		`SELECT status, last_error FROM delivery_outbox WHERE turn_id = 140 AND position = 1`).
		Scan(&status, &lastError); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "cancelled" || lastError == nil {
		t.Fatalf("устаревший хвостовой пузырь должен быть отменён: %s %v", status, lastError)
	}
	if len(stub.sentMessages()) != 0 {
		t.Fatalf("устаревшее не должно было уйти в сеть")
	}
}

// TestDeliveryLoopStalenessSeesBufferedNextTurn — M3: к моменту доставки
// новые сообщения обычно уже buffered в следующем ходе. Старая проверка
// (`pending AND turn_id IS NULL`) их не видела, и хвостовые пузыри уходили.
func TestDeliveryLoopStalenessSeesBufferedNextTurn(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)

	var nextTurn int64
	if err := pool.QueryRow(ctx, `INSERT INTO turns (chat_id) VALUES (42) RETURNING id`).Scan(&nextTurn); err != nil {
		t.Fatalf("следующий ход не создан: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, kind, text, raw_update, status, turn_id)
		VALUES (9201, 42, 'private', 'message', 'новое сообщение', '{}', 'buffered', $1)`, nextTurn); err != nil {
		t.Fatalf("buffered-событие следующего хода не вставлено: %v", err)
	}
	insertOutboxSent(t, pool, 142, 42, 0, `{"kind":"message","text":"прямой ответ"}`)
	insertOutbox(t, pool, 142, 42, 1, `{"kind":"message","text":"уже неуместно"}`)

	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM delivery_outbox WHERE turn_id = 142 AND position = 1`).Scan(&status); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("хвостовой пузырь при buffered-событии следующего хода: %s, хочу cancelled", status)
	}
	if len(stub.sentMessages()) != 0 {
		t.Fatalf("устаревшее не должно было уйти в сеть")
	}
}

// TestDeliveryLoopStalenessKeepsFirstBubble — первый пузырь (позиция 0) —
// прямой ответ, ради которого ход состоялся: уходит всегда, даже если чат
// ушёл вперёд (портировано из TS-линии c0a986d, 2026-10-31).
func TestDeliveryLoopStalenessKeepsFirstBubble(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, kind, raw_update, status)
		VALUES (9101, 42, 'private', 'message', '{}', 'pending')`); err != nil {
		t.Fatalf("pending-событие не вставлено: %v", err)
	}
	insertOutbox(t, pool, 141, 42, 0, `{"kind":"message","text":"прямой ответ"}`)

	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM delivery_outbox WHERE turn_id = 141 AND position = 0`).Scan(&status); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "sent" {
		t.Fatalf("первый пузырь должен уйти при устаревании чата, статус %s", status)
	}
	if len(stub.sentMessages()) != 1 {
		t.Fatalf("первый пузырь не дошёл до сети: %v", stub.sentMessages())
	}
}

// TestDeliveryCanceledBeforeSendRequeues — C1: остановка процесса после захвата,
// но до отправки, не оставляет строку в sending. Исход пишется на отдельном
// контексте (отмена цикла его не убивает), строка возвращается в очередь, а не
// хоронится стартом в unknown вместе со всем хвостом хода.
func TestDeliveryCanceledBeforeSendRequeues(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)

	id := insertOutbox(t, pool, 190, 42, 0, `{"kind":"message","text":"не должен потеряться"}`)
	claimed := mustClaim(t, repo, 10)
	if len(claimed) != 1 {
		t.Fatalf("захвачено %d строк, хочу 1", len(claimed))
	}

	// Остановка: ctx уже отменён до попытки отправки.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{})
	loop.deliver(canceled, claimed[0])

	var status string
	var lastError *string
	if err := pool.QueryRow(ctx,
		`SELECT status, last_error FROM delivery_outbox WHERE id = $1`, id).
		Scan(&status, &lastError); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "retry_wait" {
		t.Fatalf("после остановки до отправки строка в %q, хочу retry_wait (не sending/unknown)", status)
	}
	if len(stub.sentMessages()) != 0 {
		t.Fatalf("при отменённом ctx отправки быть не должно")
	}
}

// TestDeliveryTickCancellationDuringBubbleGapRequeues — C1: остановка во время
// паузы между пузырями возвращает строку в очередь (requeueAbandoned), а не
// оставляет её в sending до recovery/unknown.
func TestDeliveryTickCancellationDuringBubbleGapRequeues(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())
	repo := NewOutboxRepo(pool)

	id := insertOutbox(t, pool, 195, 42, 1, `{"kind":"message","text":"второй пузырь"}`)
	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{BubbleGap: time.Hour})
	// Первый пузырь уже отправлен — пауза перед вторым длинная.
	loop.lastSent[195] = time.Now()

	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := loop.Tick(runCtx); err == nil {
		t.Fatal("такт при отмене должен вернуть ошибку контекста")
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM delivery_outbox WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "retry_wait" {
		t.Fatalf("после отмены в паузе статус %q, хочу retry_wait", status)
	}
	if len(stub.sentMessages()) != 0 {
		t.Fatalf("недоставленное не должно уходить в сеть")
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
	// reaction — список ReactionType (параметр «emoji» Bot API игнорирует).
	if len(stub.reactions) != 1 || stub.reactions[0]["message_id"].(float64) != 7 {
		t.Fatalf("setMessageReaction не дошёл: %v", stub.reactions)
	}
	reactionList, ok := stub.reactions[0]["reaction"].([]any)
	if !ok || len(reactionList) != 1 {
		t.Fatalf("reaction должен быть списком из одного: %v", stub.reactions[0]["reaction"])
	}
	reaction, ok := reactionList[0].(map[string]any)
	if !ok || reaction["type"] != "emoji" || reaction["emoji"] != "🔥" {
		t.Fatalf("ReactionTypeEmoji: %v", reactionList[0])
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

// TestDeliveryLoopDBErrorBeforeSendRequeues — 16.3: ошибка БД до попытки
// отправки (перепроверка статуса StatusOf) не оставляет строку в sending до
// рестарта: захват возвращается в retry_wait и берётся повторно. Обрыв
// соединения — реальный сценарий: единственное соединение пула доставки
// убивается на стороне PostgreSQL (pg_terminate_backend, как в тестах
// singleton §5.1), StatusOf падает на мёртвом соединении, следующий запрос
// пула получает свежее.
func TestDeliveryLoopDBErrorBeforeSendRequeues(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())

	// Пул доставки из одного соединения без фоновых проверек здоровья:
	// после обрыва первый запрос (StatusOf) обязан упасть на мёртвом
	// соединении, следующий (возврат строки) — получить свежее.
	singleCfg, err := pgxpool.ParseConfig(pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatalf("конфиг пула не разбирается: %v", err)
	}
	singleCfg.MaxConns = 1
	singleCfg.MinConns = 0
	singleCfg.HealthCheckPeriod = time.Hour
	single, err := runtime.NewPoolFromConfig(ctx, singleCfg)
	if err != nil {
		t.Fatalf("пул доставки не создан: %v", err)
	}
	t.Cleanup(single.Close)
	repo := NewOutboxRepo(single)

	id := insertOutbox(t, pool, 180, 42, 0, `{"kind":"message","text":"пережить обрыв базы"}`)
	claimed := mustClaim(t, repo, 10)
	if len(claimed) != 1 {
		t.Fatalf("захвачено %d строк, хочу 1", len(claimed))
	}

	// Обрыв единственного соединения пула доставки со стороны СУБД.
	var backendPID int32
	if err := single.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		t.Fatalf("pid соединения не читается: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, backendPID); err != nil {
		t.Fatalf("соединение не оборвано: %v", err)
	}

	// Отправка захваченной строки: StatusOf падает на мёртвом соединении.
	// Пауза повтора мала (RetryBase), чтобы свежий такт сразу взял строку.
	loop := NewDeliveryLoop(pool, repo, client, nil, DeliveryOptions{RetryBase: 50 * time.Millisecond})
	loop.deliver(ctx, claimed[0])

	var status string
	var notBefore time.Time
	var lastError *string
	if err := pool.QueryRow(ctx,
		`SELECT status, not_before, last_error FROM delivery_outbox WHERE id = $1`, id).
		Scan(&status, &notBefore, &lastError); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "retry_wait" {
		t.Fatalf("после ошибки БД до отправки строка в %q, хочу retry_wait — пузырь зависает в sending до рестарта", status)
	}
	if lastError == nil || !strings.Contains(*lastError, "ошибка БД") {
		t.Fatalf("причина возврата в retry_wait не записана: %v", lastError)
	}
	if notBefore.IsZero() {
		t.Fatalf("пауза повтора (not_before) не записана")
	}

	// Строка берётся повторно: свежий такт с живым пулом отправляет её.
	time.Sleep(80 * time.Millisecond) // пауза повтора (RetryBase) истекла
	fresh := NewDeliveryLoop(pool, NewOutboxRepo(pool), client, nil, DeliveryOptions{})
	if err := fresh.Tick(ctx); err != nil {
		t.Fatalf("повторный такт не удался: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM delivery_outbox WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "sent" {
		t.Fatalf("после повтора строка в %s, хочу sent", status)
	}
	if sent := stub.sentMessages(); len(sent) != 1 || sent[0]["text"] != "пережить обрыв базы" {
		t.Fatalf("сообщение не доставлено повторно: %v", sent)
	}
}

// TestDeliveryLoopPeriodicRecovery — 16.3: recovery зависшего sending идёт
// периодически из такта доставки (§12.2), а не раз при старте процесса:
// пузырь, зависший в живом процессе, подбирается без рестарта. Пока интервал
// recovery не истёк, такт его не запускает (обычный такт не должен бегать
// recovery-запросом в БД) — включая второй такт сразу после срабатывания:
// счётчик троттлина обновляется, интервал отсчитывается заново (без этого
// после первого интервала recovery деградировал бы до «каждым тактом»,
// ~×60 нагрузки). Порог staleness — 1 минута, свежее sending и unknown-строки
// не отправляются вслепую (образец: TestRecoverStaleSending).
func TestDeliveryLoopPeriodicRecovery(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	stub := &telegramStub{}
	server := stub.server(t)
	client := NewClient("TESTTOKEN", server.URL, server.Client())

	// Зависшее sending (5 минут назад) и свежее sending живого процесса.
	stale := insertOutbox(t, pool, 190, 42, 0, `{"kind":"message","text":"завис процесса"}`)
	fresh := insertOutbox(t, pool, 191, 43, 0, `{"kind":"message","text":"живой процесс"}`)
	if _, err := pool.Exec(ctx, `
		UPDATE delivery_outbox SET status = 'sending', attempts = 1, updated_at = now() - interval '5 minutes'
		WHERE id = $1`, stale); err != nil {
		t.Fatalf("зависшее sending не выставлено: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE delivery_outbox SET status = 'sending', attempts = 1
		WHERE id = $1`, fresh); err != nil {
		t.Fatalf("свежее sending не выставлено: %v", err)
	}

	// Интервал recovery не истёк — такт его не запускает.
	idle := NewDeliveryLoop(pool, NewOutboxRepo(pool), client, nil, DeliveryOptions{})
	if err := idle.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM delivery_outbox WHERE id = $1`, stale).Scan(&status); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "sending" {
		t.Fatalf("до истечения интервала recovery тронул зависшую строку: %s", status)
	}

	// Интервал истёк — такт подбирает зависшее sending. Отсчёт сдвинут в
	// прошлое: между сборкой цикла и тактом нет ни одного оператора, а
	// монотонные часы Windows грубее наносекунды — time.Since измерялся нулём
	// и recovery не запускался (находка зелёного прогона). Интервал 10 с:
	// между тактами теста проходят миллисекунды — второй такт ниже честно
	// считает интервал неистёкшим.
	active := NewDeliveryLoop(pool, NewOutboxRepo(pool), client, nil,
		DeliveryOptions{RecoveryInterval: 10 * time.Second})
	active.lastRecovery = time.Now().Add(-20 * time.Second)
	if err := active.Tick(ctx); err != nil {
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
	if statuses[stale] != "unknown" {
		t.Fatalf("зависшее sending не подобрано периодическим recovery: %s", statuses[stale])
	}
	if statuses[fresh] != "sending" {
		t.Fatalf("свежее sending затронуто recovery: %s", statuses[fresh])
	}
	if len(stub.sentMessages()) != 0 {
		t.Fatalf("recovery не должен отправлять вслепую (§13.2): %v", stub.sentMessages())
	}

	// Второй такт сразу после срабатывания: счётчик троттлина обновился,
	// интервал отсчитывается заново — новая зависшая строка ждёт следующего
	// интервала, а не подбирается каждым тактом (мутация М6 приёмки).
	secondStale := insertOutbox(t, pool, 192, 44, 0, `{"kind":"message","text":"ждёт интервала"}`)
	if _, err := pool.Exec(ctx, `
		UPDATE delivery_outbox SET status = 'sending', attempts = 1, updated_at = now() - interval '5 minutes'
		WHERE id = $1`, secondStale); err != nil {
		t.Fatalf("второе зависшее sending не выставлено: %v", err)
	}
	if err := active.Tick(ctx); err != nil {
		t.Fatalf("второй такт не удался: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM delivery_outbox WHERE id = $1`, secondStale).Scan(&status); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "sending" {
		t.Fatalf("recovery бежит каждым тактом после первого срабатывания (счётчик троттлина не обновился): %s", status)
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

// TestDeliveryLoopSendsSticker — kind=sticker из outbox уходит sendSticker с
// file_id из коллекции (этап 25.4); message_id стикера записывается в sent.
func TestDeliveryLoopSendsSticker(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	var stickerBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTESTTOKEN/sendSticker" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&stickerBody)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":555}}`))
	}))
	defer server.Close()
	client := NewClient("TESTTOKEN", server.URL, server.Client())

	id := insertOutbox(t, pool, 150, 42, 0, `{"kind":"sticker","file_id":"STICKER-FILE","emoji":"😂"}`)
	loop := NewDeliveryLoop(pool, NewOutboxRepo(pool), client, nil, DeliveryOptions{BubbleGap: time.Millisecond})
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}
	if stickerBody["sticker"] != "STICKER-FILE" {
		t.Fatalf("стикер отправлен с %v, хочу sticker=STICKER-FILE", stickerBody)
	}
	var status string
	var telegramID *int64
	if err := pool.QueryRow(ctx,
		`SELECT status, telegram_message_id FROM delivery_outbox WHERE id = $1`, id).
		Scan(&status, &telegramID); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if status != "sent" || telegramID == nil || *telegramID != 555 {
		t.Fatalf("стикер: статус %q message_id %v, хочу sent/555", status, telegramID)
	}
}
