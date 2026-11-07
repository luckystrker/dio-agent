//go:build db

// m4 ревью 2026-11-07: ключевая гарантия whitelist'а «модель не вызывается» —
// poller не отдаёт journal_only-события в onPending (буферы → воркер).
// Дубль пачки не порождает второй onPending и не откатывает offset в памяти.
package telegram

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// batchStubAPI отдаёт заранее заданную пачку updates ровно batchLeft раз,
// затем блокируется до отмены контекста (как long poll без новых событий).
type batchStubAPI struct {
	API // остальные методы не нужны: poller зовёт только GetUpdates

	mu        sync.Mutex
	batch     []Update
	batchLeft int
	offsets   []int64
}

func (s *batchStubAPI) GetUpdates(ctx context.Context, offset int64, _ int) ([]Update, error) {
	s.mu.Lock()
	s.offsets = append(s.offsets, offset)
	if s.batchLeft > 0 {
		s.batchLeft--
		out := append([]Update(nil), s.batch...)
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *batchStubAPI) calls() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.offsets...)
}

// TestPollerPrivateAllowlistNoPendingAndDuplicateBatch — чужое сообщение и его
// дубль не доходят до onPending (хода нет), своё — доходит ровно один раз;
// offset в памяти после дубля остаётся update_id+1.
func TestPollerPrivateAllowlistNoPendingAndDuplicateBatch(t *testing.T) {
	pool := newTestPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const friend, stranger = int64(722958928), int64(42)
	stub := &batchStubAPI{
		batch: []Update{
			{UpdateID: 900, Raw: json.RawMessage(rawMessageOf(900, stranger, "привет"))},
			{UpdateID: 901, Raw: json.RawMessage(rawMessageOf(901, friend, "своё"))},
		},
		batchLeft: 2, // первая выдача и дубль той же пачки
	}

	var mu sync.Mutex
	var pending []int64
	poller := NewTelegramPoller(pool, stub, func(ev PendingEvent) {
		mu.Lock()
		pending = append(pending, ev.ChatID)
		mu.Unlock()
	}, nil, PollerOptions{PollTimeout: 50 * time.Millisecond, ErrorBackoff: 20 * time.Millisecond})
	poller.SetPrivateAllowlist(NewPrivateAllowlist([]int64{friend}))

	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()

	// Третий getUpdates означает: обе выдачи (включая дубль) обработаны.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if len(stub.calls()) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("poller не обработал пачки за 10 с, offsets=%v", stub.calls())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poller не остановился")
	}

	mu.Lock()
	gotPending := append([]int64(nil), pending...)
	mu.Unlock()
	if len(gotPending) != 1 || gotPending[0] != friend {
		t.Fatalf("onPending получил %v, хочу только разрешённую личку %d (journal_only в буфер не идёт)",
			gotPending, friend)
	}

	// ctx уже отменён — проверки БД идут по фону.
	dbCtx := context.Background()
	// Дубль не создал вторых событий: два update_id — две строки журнала.
	var events int
	if err := pool.QueryRow(dbCtx, `SELECT count(*) FROM social_events`).Scan(&events); err != nil || events != 2 {
		t.Fatalf("событий в журнале: %d (err=%v), хочу 2 (дубль отсечён UNIQUE)", events, err)
	}
	var status string
	if err := pool.QueryRow(dbCtx, `SELECT status FROM social_events WHERE telegram_update_id = 900`).Scan(&status); err != nil || status != string(StatusJournalOnly) {
		t.Fatalf("чужое сообщение: статус %q (err=%v), хочу journal_only", status, err)
	}

	// offset в БД — за последним update; в памяти poller тоже ушёл вперёд.
	if offset, has, err := ReadOffset(dbCtx, pool); err != nil || !has || offset != 902 {
		t.Fatalf("offset в БД = %d (has=%v err=%v), хочу 902", offset, has, err)
	}
	calls := stub.calls()
	if calls[len(calls)-1] < 902 {
		t.Fatalf("poller запрашивал getUpdates с offset %v — после дубля ожидал ≥902", calls)
	}
}
