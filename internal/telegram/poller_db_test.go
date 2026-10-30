//go:build db

// БД-тест связки poller → ingest → буферы (§6.1): пачка сохраняется по
// порядку, offset после записи, pending-события попадают в буфер, повторный
// прогон не создаёт дублей.
package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
