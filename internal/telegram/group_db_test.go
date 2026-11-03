//go:build db

// БД-тесты групп (этап 30): гейт закрытой пачки, вовлечённость в
// chat_involvement и потолок встреваний.
package telegram

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

func rawGroupMessageOf(updateID, chatID int64, text string) []byte {
	return []byte(fmt.Sprintf(`{"update_id":%d,"message":{"message_id":%d,"from":{"id":7,"first_name":"Аня"},"chat":{"id":%d,"type":"group"},"text":%q}}`,
		updateID, updateID, chatID, text))
}

func newGroupGateForTest(pool *runtime.Pool, now time.Time) *GroupGate {
	gate := NewGroupGate(pool, GroupGateConfig{
		Name:     "Дио",
		Aliases:  []string{"дийо"},
		Topics:   []string{"старые пластинки и треск"},
		Embedder: memory.NewHashEmbedder(),
		Now:      func() time.Time { return now },
	})
	gate.Prepare(context.Background())
	return gate
}

func withShortGroupWindows(m *ChatBufferManager) {
	m.Windows = func(ChatType, EventKind) Windows {
		return Windows{Idle: 30 * time.Millisecond, Max: time.Second}
	}
}

// waitBufferClose ждёт закрытия пачки: OnTurnReady приходит только при
// созданном ходе, поэтому для journal_only достаточно небольшой паузы.
func waitBufferClose(t *testing.T, m *ChatBufferManager, ready <-chan struct{}, expectTurn bool) {
	t.Helper()
	if expectTurn {
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Fatalf("ход не создан за 3 с")
		}
		m.Wait()
		return
	}
	time.Sleep(300 * time.Millisecond)
	m.Wait()
}

func TestGroupGatePassiveNoiseJournalOnly(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	now := time.Now()

	ev := mustIngest(t, pool, rawGroupMessageOf(9001, -100, "сегодня опять дождь и слякоть"))
	ready := make(chan struct{}, 1)
	m := NewChatBufferManager(BufferDeps{Pool: pool, Gate: newGroupGateForTest(pool, now),
		OnTurnReady: func(int64, int64, string) { ready <- struct{}{} }})
	withShortGroupWindows(m)
	m.Add(PendingEvent{EventID: ev.EventID, ChatID: -100, ChatType: ChatGroup, Kind: KindMessage, Text: ev.Event.Text})
	waitBufferClose(t, m, ready, false)

	var turnID *int64
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT turn_id, status FROM social_events WHERE id = $1`, ev.EventID).Scan(&turnID, &status); err != nil {
		t.Fatalf("событие не читается: %v", err)
	}
	if turnID != nil || status != string(StatusJournalOnly) {
		t.Fatalf("пассивный шум группы: turn=%v status=%s, хочу journal_only без хода", turnID, status)
	}
}

func TestGroupGateNameStartsDirectTurn(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	now := time.Now()

	ev := mustIngest(t, pool, rawGroupMessageOf(9002, -101, "Дио, привет"))
	ready := make(chan struct{}, 1)
	m := NewChatBufferManager(BufferDeps{Pool: pool, Gate: newGroupGateForTest(pool, now),
		OnTurnReady: func(int64, int64, string) { ready <- struct{}{} }})
	withShortGroupWindows(m)
	m.Add(PendingEvent{EventID: ev.EventID, ChatID: -101, ChatType: ChatGroup, Kind: KindMessage, Text: ev.Event.Text})
	waitBufferClose(t, m, ready, true)

	var turnID *int64
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT turn_id, status FROM social_events WHERE id = $1`, ev.EventID).Scan(&turnID, &status); err != nil {
		t.Fatalf("событие не читается: %v", err)
	}
	if turnID == nil || status != "buffered" {
		t.Fatalf("имя бота не создало ход: turn=%v status=%s", turnID, status)
	}
	// Прямое обращение сбрасывает потолок вовлечённости (§30.2).
	var ceiling time.Time
	if err := pool.QueryRow(ctx,
		`SELECT ceiling_until FROM chat_involvement WHERE chat_id = -101`).Scan(&ceiling); err != nil {
		t.Fatalf("потолок вовлечённости не записан: %v", err)
	}
	if ceiling.Before(now.Add(GroupInvolvementCeiling - time.Minute)) {
		t.Fatalf("потолок не сброшен: %v", ceiling)
	}
}

func TestGroupGateInvolvedChatStartsTurn(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	now := time.Now()

	if err := InvolveAfterBotAction(ctx, pool, -102, now); err != nil {
		t.Fatalf("вовлечённость не отмечена: %v", err)
	}
	ev := mustIngest(t, pool, rawGroupMessageOf(9003, -102, "ну и что дальше"))
	ready := make(chan struct{}, 1)
	m := NewChatBufferManager(BufferDeps{Pool: pool, Gate: newGroupGateForTest(pool, now),
		OnTurnReady: func(int64, int64, string) { ready <- struct{}{} }})
	withShortGroupWindows(m)
	m.Add(PendingEvent{EventID: ev.EventID, ChatID: -102, ChatType: ChatGroup, Kind: KindMessage, Text: ev.Event.Text})
	waitBufferClose(t, m, ready, true)

	var turnID *int64
	if err := pool.QueryRow(ctx,
		`SELECT turn_id FROM social_events WHERE id = $1`, ev.EventID).Scan(&turnID); err != nil {
		t.Fatalf("событие не читается: %v", err)
	}
	if turnID == nil {
		t.Fatalf("сообщение вовлечённого чата не пошло в ход")
	}
}

func TestGroupGateUnpromptedCooldown(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	now := time.Now()
	gate := newGroupGateForTest(pool, now)

	first := mustIngest(t, pool, rawGroupMessageOf(9004, -103, "старые пластинки и треск"))
	decision, err := gate.DecideBatch(ctx, -103, []int64{first.EventID})
	if err != nil || !decision.Process {
		t.Fatalf("первая цепляющая тема: %+v err=%v", decision, err)
	}

	second := mustIngest(t, pool, rawGroupMessageOf(9005, -103, "старые пластинки и треск"))
	decision, err = gate.DecideBatch(ctx, -103, []int64{second.EventID})
	if err != nil {
		t.Fatalf("вторая пачка: %v", err)
	}
	if decision.Process {
		t.Fatalf("потолок встреваний 20 минут не сработал: %+v", decision)
	}

	// Через 21 минуту — снова можно.
	gate.now = func() time.Time { return now.Add(21 * time.Minute) }
	decision, err = gate.DecideBatch(ctx, -103, []int64{second.EventID})
	if err != nil || !decision.Process {
		t.Fatalf("после потолка тема не зацепила: %+v err=%v", decision, err)
	}
}

func TestInvolvementWindowCeilingAndReset(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	now := time.Now()
	const chatID = -200

	// Ответ бота: окно 5 минут, потолок 30.
	if err := InvolveAfterBotAction(ctx, pool, chatID, now); err != nil {
		t.Fatalf("ответ бота: %v", err)
	}
	involved, err := GroupInvolved(ctx, pool, chatID, now.Add(4*time.Minute))
	if err != nil || !involved {
		t.Fatalf("через 4 мин чат не вовлечён: %v err=%v", involved, err)
	}
	involved, err = GroupInvolved(ctx, pool, chatID, now.Add(6*time.Minute))
	if err != nil || involved {
		t.Fatalf("через 6 мин окно не истекло: %v err=%v", involved, err)
	}

	// Сообщение в живом окне продлевает его.
	if err := ExtendGroupInvolvement(ctx, pool, chatID, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("продление: %v", err)
	}
	involved, err = GroupInvolved(ctx, pool, chatID, now.Add(8*time.Minute))
	if err != nil || !involved {
		t.Fatalf("окно не продлено: %v err=%v", involved, err)
	}

	// Потолок 30 минут подряд без прямого обращения не продлевается.
	gateway := now.Add(29 * time.Minute)
	if err := ExtendGroupInvolvement(ctx, pool, chatID, gateway); err == nil {
		// окно к этому моменту уже истекло — UPDATE без строк не ошибка
	}
	if err := InvolveAfterBotAction(ctx, pool, chatID, gateway); err != nil {
		t.Fatalf("ответ бота у потолка: %v", err)
	}
	involved, err = GroupInvolved(ctx, pool, chatID, now.Add(31*time.Minute))
	if err != nil || involved {
		t.Fatalf("потолок 30 минут не сработал: %v err=%v", involved, err)
	}

	// Прямое обращение сбрасывает потолок: новый ответ бота снова вовлекает.
	if err := ResetGroupCeiling(ctx, pool, chatID, now.Add(31*time.Minute)); err != nil {
		t.Fatalf("сброс потолка: %v", err)
	}
	if err := InvolveAfterBotAction(ctx, pool, chatID, now.Add(31*time.Minute)); err != nil {
		t.Fatalf("ответ после сброса: %v", err)
	}
	involved, err = GroupInvolved(ctx, pool, chatID, now.Add(35*time.Minute))
	if err != nil || !involved {
		t.Fatalf("после прямого обращения чат не вовлечён: %v err=%v", involved, err)
	}
}

func TestUnpromptedCooldownState(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	now := time.Now()
	const chatID = -300

	allowed, err := GroupUnpromptedAllowed(ctx, pool, chatID, now)
	if err != nil || !allowed {
		t.Fatalf("без отметки встревание запрещено: %v err=%v", allowed, err)
	}
	if err := TouchGroupUnprompted(ctx, pool, chatID, now); err != nil {
		t.Fatalf("отметка встревания: %v", err)
	}
	allowed, err = GroupUnpromptedAllowed(ctx, pool, chatID, now.Add(10*time.Minute))
	if err != nil || allowed {
		t.Fatalf("встревание раньше 20 мин не запрещено: %v err=%v", allowed, err)
	}
	allowed, err = GroupUnpromptedAllowed(ctx, pool, chatID, now.Add(21*time.Minute))
	if err != nil || !allowed {
		t.Fatalf("после 21 мин встревание не разрешено: %v err=%v", allowed, err)
	}
}
