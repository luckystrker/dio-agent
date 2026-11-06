//go:build db

// БД-тесты реакций пользователя на сообщения бота (34.1): текст события с
// цитатой ответа, journal_only для чужих сообщений и групп без вовлечённости,
// окно сбора и низкий приоритет хода, потолок один ход в 10 минут на чат.
package telegram

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// insertSentOutbox вставляет sent-строку ответа бота (цель реакции).
func insertSentOutbox(t *testing.T, ctx context.Context, pool *runtime.Pool, chatID, messageID int64, kind, text, emoji string) {
	t.Helper()
	payload := fmt.Sprintf(`{"kind":%q,"text":%q,"emoji":%q}`, kind, text, emoji)
	if _, err := pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, telegram_message_id)
		VALUES (0, 0, $1, $2::jsonb, 'sent', $3)`, chatID, payload, messageID); err != nil {
		t.Fatalf("sent-ответ бота не вставлен: %v", err)
	}
}

// rawReactionOf — update message_reaction (old/new — эмодзи; пусто — нет).
func rawReactionOf(updateID, chatID, messageID int64, oldEmoji, newEmoji string) []byte {
	reactions := func(emoji string) string {
		if emoji == "" {
			return "[]"
		}
		return fmt.Sprintf(`[{"type":"emoji","emoji":%q}]`, emoji)
	}
	return []byte(fmt.Sprintf(`{"update_id":%d,"message_reaction":{"chat":{"id":%d,"type":"private"},`+
		`"message_id":%d,"user":{"id":42,"first_name":"Коля"},"date":1700000000,`+
		`"old_reaction":%s,"new_reaction":%s}}`,
		updateID, chatID, messageID, reactions(oldEmoji), reactions(newEmoji)))
}

func TestIngestReactionOnBotMessage(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	insertSentOutbox(t, ctx, pool, 42, 77, "message", "это был мой ответ", "")

	res := mustIngest(t, pool, rawReactionOf(900, 42, 77, "", "😂"))
	if !res.Inserted || res.Status != StatusPending {
		t.Fatalf("реакция на ответ бота должна идти в ход: %+v", res)
	}
	if res.Event.Kind != KindReaction || res.Event.Reaction != "😂" || !res.Event.ReactionAdded {
		t.Fatalf("событие реакции: %+v", res.Event)
	}
	var text string
	if err := pool.QueryRow(ctx, `SELECT text FROM social_events WHERE id = $1`, res.EventID).Scan(&text); err != nil {
		t.Fatalf("текст события не читается: %v", err)
	}
	if text != `[поставил 😂 на твоё «это был мой ответ»]` {
		t.Fatalf("текст события = %q", text)
	}

	// Снятие реакции — отдельное событие с той же цитатой и всегда история
	// (41.3 m28): случайно поставленная и снятая реакция не создаёт ход.
	removed := mustIngest(t, pool, rawReactionOf(901, 42, 77, "😂", ""))
	if removed.Status != StatusJournalOnly {
		t.Fatalf("снятие реакции создало ход: %+v", removed)
	}
	var removedText string
	if err := pool.QueryRow(ctx, `SELECT text FROM social_events WHERE id = $1`, removed.EventID).Scan(&removedText); err != nil {
		t.Fatalf("текст снятия не читается: %v", err)
	}
	if removedText != `[убрал реакцию 😂 с твоего «это был мой ответ»]` {
		t.Fatalf("текст снятия = %q", removedText)
	}
}

func TestIngestReactionOnForeignMessageIsJournalOnly(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	res := mustIngest(t, pool, rawReactionOf(910, 42, 500, "", "🔥"))
	if res.Status != StatusJournalOnly {
		t.Fatalf("реакция на чужое сообщение — journal_only: %+v", res)
	}
	var text string
	if err := pool.QueryRow(ctx, `SELECT text FROM social_events WHERE id = $1`, res.EventID).Scan(&text); err != nil {
		t.Fatalf("текст события не читается: %v", err)
	}
	if text != `[поставил 🔥]` {
		t.Fatalf("журнальный текст = %q", text)
	}
}

func TestIngestReactionInGroupRequiresInvolvement(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const groupID = int64(-300)
	insertSentOutbox(t, ctx, pool, groupID, 80, "message", "ответ в группе", "")

	// Бот получил reaction в группе только админом, но без вовлечённости
	// (этап 30.2) пачка моделью не читается.
	foreign := mustIngest(t, pool, []byte(`{"update_id":920,"message_reaction":{"chat":{"id":-300,"type":"group"},`+
		`"message_id":80,"user":{"id":43,"first_name":"Петя"},"old_reaction":[],"new_reaction":[{"type":"emoji","emoji":"👍"}]}}`))
	if foreign.Status != StatusJournalOnly {
		t.Fatalf("группа без вовлечённости — journal_only: %+v", foreign)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO chat_involvement (chat_id, involved_until, ceiling_until)
		VALUES ($1, now() + interval '5 minutes', now() + interval '30 minutes')`, groupID); err != nil {
		t.Fatalf("вовлечённость не создана: %v", err)
	}
	involved := mustIngest(t, pool, []byte(`{"update_id":921,"message_reaction":{"chat":{"id":-300,"type":"group"},`+
		`"message_id":80,"user":{"id":43,"first_name":"Петя"},"old_reaction":[],"new_reaction":[{"type":"emoji","emoji":"👍"}]}}`))
	if involved.Status != StatusPending {
		t.Fatalf("вовлечённая группа — pending: %+v", involved)
	}
}

func TestBufferReactionBatchLowPriority(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	insertSentOutbox(t, ctx, pool, 42, 77, "message", "ответ", "")

	first := mustIngest(t, pool, rawReactionOf(930, 42, 77, "", "😂"))
	second := mustIngest(t, pool, rawReactionOf(931, 42, 77, "", "🔥"))

	ready := make(chan string, 2)
	m := NewChatBufferManager(BufferDeps{Pool: pool, OnTurnReady: func(turnID, chatID int64, kind string) {
		ready <- kind
	}})
	m.Windows = func(ChatType, EventKind) Windows { return Windows{Idle: 30 * time.Millisecond, Max: 5 * time.Second} }
	m.Add(PendingEvent{EventID: first.EventID, ChatID: 42, ChatType: ChatPrivate, Kind: KindReaction})
	m.Add(PendingEvent{EventID: second.EventID, ChatID: 42, ChatType: ChatPrivate, Kind: KindReaction})
	m.Wait()

	select {
	case kind := <-ready:
		if kind != "group" {
			t.Fatalf("ход по реакциям — низкий приоритет (как группа), kind=%s", kind)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("ход по реакциям не создан")
	}
	select {
	case kind := <-ready:
		t.Fatalf("несколько реакций подряд должны быть одной пачкой, второй ход kind=%s", kind)
	case <-time.After(100 * time.Millisecond):
	}

	var turnID int64
	if err := pool.QueryRow(ctx, `SELECT turn_id FROM social_events WHERE id = $1`, first.EventID).Scan(&turnID); err != nil {
		t.Fatalf("turn_id не читается: %v", err)
	}
	var bound int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM social_events WHERE turn_id = $1`, turnID).Scan(&bound); err != nil || bound != 2 {
		t.Fatalf("в реакции-ходе %d событий (err=%v), хочу 2", bound, err)
	}
}

func TestBufferReactionTurnCappedPerChat(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	insertSentOutbox(t, ctx, pool, 77, 88, "message", "ответ для потолка", "")

	first := mustIngest(t, pool, rawReactionOf(940, 77, 88, "", "😂"))
	ready := make(chan struct{}, 1)
	m := NewChatBufferManager(BufferDeps{Pool: pool, OnTurnReady: func(turnID, chatID int64, kind string) {
		ready <- struct{}{}
	}})
	m.Windows = func(ChatType, EventKind) Windows { return Windows{Idle: 20 * time.Millisecond, Max: 5 * time.Second} }
	m.Add(PendingEvent{EventID: first.EventID, ChatID: 77, ChatType: ChatPrivate, Kind: KindReaction})
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatalf("первый ход по реакции не создан")
	}

	second := mustIngest(t, pool, rawReactionOf(941, 77, 88, "", "🔥"))
	m.Add(PendingEvent{EventID: second.EventID, ChatID: 77, ChatType: ChatPrivate, Kind: KindReaction})

	var status string
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := pool.QueryRow(ctx, `SELECT status FROM social_events WHERE id = $1`, second.EventID).Scan(&status); err != nil {
			t.Fatalf("статус второй реакции не читается: %v", err)
		}
		if status != "pending" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != "journal_only" {
		t.Fatalf("вторая реакция в 10 минут — только история (journal_only), статус %s (34.1)", status)
	}
	var turns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE chat_id = 77`).Scan(&turns); err != nil || turns != 1 {
		t.Fatalf("ходов по реакциям в чате: %d (err=%v), хочу 1", turns, err)
	}
}
