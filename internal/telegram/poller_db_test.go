//go:build db

// БД-тест связки poller → ingest → буферы (§6.1): пачка сохраняется по
// порядку, offset после записи, pending-события попадают в буфер, повторный
// прогон не создаёт дублей.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestPollerIngestsBatchAndFeedsBuffers(t *testing.T) {
	pool := newTestPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := []string{
		`{"update_id":1000,"message":{"message_id":1,"from":{"id":42,"first_name":"Коля"},"chat":{"id":42,"type":"private"},"text":"раз"}}`,
		`{"update_id":1001,"message":{"message_id":2,"from":{"id":42,"first_name":"Коля"},"chat":{"id":42,"type":"private"},"text":"два"}}`,
		`{"update_id":1002,"message":{"message_id":3,"from":{"id":7,"first_name":"П"},"chat":{"id":-5,"type":"group"},"text":"шум группы"}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Offset int64 `json:"offset"`
		}
		_ = json.Unmarshal(raw, &body)
		out := []json.RawMessage{}
		for _, u := range updates {
			var id struct {
				UpdateID int64 `json:"update_id"`
			}
			_ = json.Unmarshal([]byte(u), &id)
			if id.UpdateID >= body.Offset {
				out = append(out, json.RawMessage(u))
			}
		}
		resp, _ := json.Marshal(map[string]any{"ok": true, "result": out})
		_, _ = w.Write(resp)
	}))
	t.Cleanup(server.Close)

	pending := make(chan PendingEvent, 16)
	m := NewChatBufferManager(BufferDeps{
		Pool: pool,
		OnTurnReady: func(turnID, chatID int64, kind string) {
			pending <- PendingEvent{EventID: turnID} // сигнал о закрытии порции
		},
	})
	m.Windows = func(ChatType, EventKind) Windows { return Windows{Idle: 40 * time.Millisecond, Max: time.Second} }

	poller := NewTelegramPoller(pool, NewClient("T", server.URL, server.Client()),
		func(ev PendingEvent) { m.Add(ev) }, nil,
		PollerOptions{PollTimeout: 100 * time.Millisecond, ErrorBackoff: 10 * time.Millisecond})

	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()

	// Ждём закрытия порции лички (оба сообщения в одном turn).
	deadline := time.After(5 * time.Second)
	var turnSignals int
	for turnSignals < 1 {
		select {
		case <-pending:
			turnSignals++
		case <-deadline:
			t.Fatalf("порция не закрылась за 5 с")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("poller остановился с ошибкой: %v", err)
	}
	m.Wait()

	// Все три update сохранены (включая шум группы journal_only), offset — 1003.
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM social_events`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("сохранено %d событий (err=%v), хочу 3", count, err)
	}
	offset, _, err := ReadOffset(context.Background(), pool)
	if err != nil || offset != 1003 {
		t.Fatalf("offset = %d (err=%v), хочу 1003", offset, err)
	}
	// Порция лички закрыта в turn, шум группы остался journal_only.
	var buffered, journaled int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM social_events WHERE status = 'buffered'`).Scan(&buffered); err != nil || buffered != 2 {
		t.Fatalf("buffered: %d (err=%v), хочу 2", buffered, err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM social_events WHERE status = 'journal_only'`).Scan(&journaled); err != nil || journaled != 1 {
		t.Fatalf("journal_only: %d (err=%v), хочу 1", journaled, err)
	}
	// Однократность: повторный прогон poller по тому же offset не создаёт дублей
	// (проверяем механику ingest — restart с offset 0 отсечётся UNIQUE).
	poller2 := NewTelegramPoller(pool, NewClient("T", server.URL, server.Client()), nil, nil,
		PollerOptions{PollTimeout: 50 * time.Millisecond, ErrorBackoff: 5 * time.Millisecond})
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { _ = poller2.Run(ctx2) }()
	time.Sleep(300 * time.Millisecond)
	cancel2()
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM social_events`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("повторный прогон создал дубли: %d событий (err=%v)", count, err)
	}
}

// TestPollerFatalOnDeadToken — 401 от getUpdates фатален (16.4): протухший
// токен — честный останов процесса под systemd (app делает fatalFail по
// ошибке Run), а не вечная тишина при зелёном heartbeat.
func TestPollerFatalOnDeadToken(t *testing.T) {
	pool := newTestPool(t)
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	})
	poller := NewTelegramPoller(pool, client, nil, nil,
		PollerOptions{PollTimeout: 50 * time.Millisecond, ErrorBackoff: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- poller.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("401: Run вернул nil — тихая глухота вместо ошибки")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus != 401 {
			t.Fatalf("ошибка должна нести APIError 401: %v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("401 должен останавливать сразу, а не после пауз: %v", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("401: Run не вернул ошибку за 5 с — polling глухо крутится с паузами")
	}
}

// TestPollerSkipsUnusableUpdates — ядовитый update не глушит приём (16.4):
// NUL в text (\u0000 — PostgreSQL не принимает его в text/jsonb) и update
// без update_id — ошибки данных, повтор пачки не лечит; такой update
// скипается с продвижением offset, остальные события пачки обрабатываются.
func TestPollerSkipsUnusableUpdates(t *testing.T) {
	pool := newTestPool(t)
	updates := []json.RawMessage{
		json.RawMessage(rawMessageOf(2000, 42, "до яда")),
		json.RawMessage(`{"update_id":2001,"message":{"message_id":2,"from":{"id":42,"first_name":"Коля"},"chat":{"id":42,"type":"private"},"text":"яд \u0000 в тексте"}}`),
		json.RawMessage(`{"message":{"message_id":3,"from":{"id":42,"first_name":"Коля"},"chat":{"id":42,"type":"private"},"text":"без update_id"}}`),
		json.RawMessage(rawMessageOf(2003, 42, "после яда")),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Offset int64 `json:"offset"`
		}
		_ = json.Unmarshal(raw, &body)
		out := []json.RawMessage{}
		for _, u := range updates {
			var id struct {
				UpdateID int64 `json:"update_id"`
			}
			_ = json.Unmarshal(u, &id)
			if id.UpdateID >= body.Offset {
				out = append(out, u)
			}
		}
		resp, _ := json.Marshal(map[string]any{"ok": true, "result": out})
		_, _ = w.Write(resp)
	}))
	t.Cleanup(server.Close)

	poller := NewTelegramPoller(pool, NewClient("T", server.URL, server.Client()), nil, nil,
		PollerOptions{PollTimeout: 100 * time.Millisecond, ErrorBackoff: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()

	// Ждём: обе годных записи сохранены и offset ушёл за конец пачки.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var good int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM social_events WHERE telegram_update_id IN (2000, 2003)`).Scan(&good); err != nil {
			t.Fatalf("события не читаются: %v", err)
		}
		offset, _, err := ReadOffset(context.Background(), pool)
		if err != nil {
			t.Fatalf("offset не читается: %v", err)
		}
		if good == 2 && offset >= 2004 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ядовитый update глушит приём: годных сохранено %d/2, offset %d (хочу 2004)", good, offset)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("poller остановился с ошибкой: %v", err)
	}

	// Ядовитые в журнал не попали, годные — на месте.
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM social_events`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("сохранено %d событий (err=%v), хочу 2 — только годные", count, err)
	}
}

// TestPollerConflict409BackoffGrows — 409 (конфликт с другим потребителем
// getUpdates) не фатален и не молотит каждые 3 с вечно: пауза нарастает (16.4).
func TestPollerConflict409BackoffGrows(t *testing.T) {
	pool := newTestPool(t)
	var mu sync.Mutex
	var calls []time.Time
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`))
	})
	poller := NewTelegramPoller(pool, client, nil, nil,
		PollerOptions{PollTimeout: 50 * time.Millisecond, ErrorBackoff: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		got := len(calls)
		mu.Unlock()
		if got >= 5 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("409: getUpdates не дошёл до 5 вызовов — polling стоит")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("409 не фатален — Run вернул ошибку: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	gaps := make([]time.Duration, 0, len(calls)-1)
	for i := 1; i < len(calls); i++ {
		gaps = append(gaps, calls[i].Sub(calls[i-1]))
	}
	// ErrorBackoff 20 мс: паузы должны расти 20→40→80→160 мс. Проверка с
	// запасом на планировщик: 4-я пауза минимум вдвое больше первой.
	if len(gaps) < 4 {
		t.Fatalf("замерено %d пауз, нужно 4: %v", len(gaps), gaps)
	}
	if gaps[3] < 2*gaps[0] {
		t.Fatalf("пауза 409 не нарастает: %v", gaps)
	}
}
