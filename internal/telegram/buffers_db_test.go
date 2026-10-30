//go:build db

// БД-тесты буферов §6.2: закрытие порции транзакционно создаёт turn и
// привязывает события; recovery pending-событий после падения (§12.2).
package telegram

import (
	"context"
	"testing"
	"time"
)

func TestBufferCloseCreatesTurnTransactionally(t *testing.T) {
	// §6.2: «turn с постоянным ID, а события связываются с ним» — таймер можно
	// потерять, принадлежность событий turn — нельзя.
	pool := newTestPool(t)
	ctx := context.Background()

	first := mustIngest(t, pool, rawMessageOf(700, 42, "раз"))
	second := mustIngest(t, pool, rawMessageOf(701, 42, "два"))

	ready := make(chan struct{}, 1)
	m := NewChatBufferManager(BufferDeps{Pool: pool, OnTurnReady: func(turnID, chatID int64, kind string) {
		if chatID != 42 || kind != "direct" {
			t.Errorf("OnTurnReady: chatID=%d kind=%s", chatID, kind)
		}
		ready <- struct{}{}
	}})
	m.Windows = func(ChatType, EventKind) Windows { return Windows{Idle: 30 * time.Millisecond, Max: 5 * time.Second} }
	m.Add(PendingEvent{EventID: first.EventID, ChatID: 42, ChatType: ChatPrivate, Kind: KindMessage})
	m.Add(PendingEvent{EventID: second.EventID, ChatID: 42, ChatType: ChatPrivate, Kind: KindMessage})
	m.Wait()

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatalf("turn не создан за 3 с")
	}

	var turnID int64
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT turn_id, status FROM social_events WHERE id = $1`, first.EventID).Scan(&turnID, &status); err != nil {
		t.Fatalf("событие не читается: %v", err)
	}
	if turnID == 0 || status != "buffered" {
		t.Fatalf("событие не привязано к turn: turn=%d status=%s", turnID, status)
	}
	var bound int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE turn_id = $1`, turnID).Scan(&bound); err != nil || bound != 2 {
		t.Fatalf("в turn %d привязано %d событий (err=%v)", turnID, bound, err)
	}
	var turnChat int64
	var turnStatus string
	if err := pool.QueryRow(ctx,
		`SELECT chat_id, status FROM turns WHERE id = $1`, turnID).Scan(&turnChat, &turnStatus); err != nil || turnChat != 42 || turnStatus != "created" {
		t.Fatalf("turn: chat=%d status=%s (err=%v)", turnChat, turnStatus, err)
	}
}

func TestBufferRecoverStalePending(t *testing.T) {
	// §12.2 «Событие pending, turn_started нет — восстановить буфер»:
	// таймеры потеряны при падении, pending закрывается в turn немедленно.
	pool := newTestPool(t)
	ctx := context.Background()

	first := mustIngest(t, pool, rawMessageOf(800, 42, "падение до закрытия"))
	// Второй чат: recovery группирует по чатам. Групповой шум сюда не входит —
	// prefilter §6.3 пишет его journal_only, до буфера он не доходит.
	second := mustIngest(t, pool, rawMessageOf(801, 43, "и во второй личке"))

	m := NewChatBufferManager(BufferDeps{Pool: pool})
	chats, events, err := m.RecoverStalePending(ctx)
	if err != nil {
		t.Fatalf("recovery не удался: %v", err)
	}
	if chats != 2 || events != 2 {
		t.Fatalf("recovery: чатов %d, событий %d, хочу 2/2", chats, events)
	}

	for _, id := range []int64{first.EventID, second.EventID} {
		var turnID int64
		var status string
		if err := pool.QueryRow(ctx,
			`SELECT turn_id, status FROM social_events WHERE id = $1`, id).Scan(&turnID, &status); err != nil {
			t.Fatalf("событие %d не читается: %v", id, err)
		}
		if turnID == 0 || status != "buffered" {
			t.Fatalf("событие %d не восстановлено в turn: turn=%d status=%s", id, turnID, status)
		}
	}
	// Повторный recovery — чистый no-op: pending не осталось.
	chats, events, err = m.RecoverStalePending(ctx)
	if err != nil || chats != 0 || events != 0 {
		t.Fatalf("повторный recovery не пуст: чаты=%d события=%d err=%v", chats, events, err)
	}
}

func TestBufferFlushAllPersistsTurns(t *testing.T) {
	// Graceful shutdown: буферы закрываются немедленно, порции не теряются (§5.3).
	pool := newTestPool(t)
	res := mustIngest(t, pool, rawMessageOf(900, 42, "перед остановкой"))

	m := NewChatBufferManager(BufferDeps{Pool: pool})
	m.Windows = func(ChatType, EventKind) Windows { return Windows{Idle: time.Hour, Max: time.Hour} }
	m.Add(PendingEvent{EventID: res.EventID, ChatID: 42, ChatType: ChatPrivate, Kind: KindMessage})

	m.FlushAll(context.Background())

	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM social_events WHERE id = $1`, res.EventID).Scan(&status); err != nil || status != "buffered" {
		t.Fatalf("flushAll не закрыл порцию: status=%s (err=%v)", status, err)
	}
}
