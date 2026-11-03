// Юнит-тесты буферов §6.2: окна по типам чатов, ближайший срок таймера,
// закрытие по лимитам, max не переносится бесконечно, flushAll. Закрытие
// подменено фейком (логика окон без БД); транзакционное закрытие — в БД-тестах.
package telegram

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

type turnRecord struct {
	chatID int64
	ids    []int64
}

// newUnitTestManager — менеджер с короткими окнами и фейковым закрытием.
func newUnitTestManager(t *testing.T, idle, max time.Duration) (*ChatBufferManager, chan turnRecord) {
	t.Helper()
	closed := make(chan turnRecord, 64)
	m := NewChatBufferManager(BufferDeps{Pool: nil})
	m.Windows = func(ChatType, EventKind) Windows { return Windows{Idle: idle, Max: max} }
	m.startTurn = func(ctx context.Context, chatID int64, ids []int64) (int64, string, error) {
		closed <- turnRecord{chatID: chatID, ids: ids}
		return 1, "direct", nil
	}
	return m, closed
}

func waitForRecord(t *testing.T, closed chan turnRecord) turnRecord {
	t.Helper()
	select {
	case rec := <-closed:
		return rec
	case <-time.After(3 * time.Second):
		t.Fatalf("буфер не закрылся за 3 с")
		return turnRecord{}
	}
}

func TestDefaultWindowsByScenario(t *testing.T) {
	// §6.2 (решение 2026-11-01): личка 3с/10с — пачка человека с паузами
	// в пару секунд собирается в один ход; пассивная группа 4с/6с;
	// reply/mention/command 150мс/1с.
	cases := []struct {
		chatType ChatType
		kind     EventKind
		idleMin  time.Duration
		idleMax  time.Duration
		maxMax   time.Duration
	}{
		{ChatPrivate, KindMessage, 2500 * time.Millisecond, 3500 * time.Millisecond, 10 * time.Second},
		{ChatGroup, KindMessage, 3 * time.Second, 5 * time.Second, 6 * time.Second},
		{ChatGroup, KindReply, 0, 300 * time.Millisecond, time.Second},
		{ChatGroup, KindMention, 0, 300 * time.Millisecond, time.Second},
		{ChatGroup, KindCommand, 0, 300 * time.Millisecond, time.Second},
	}
	for _, tc := range cases {
		w := DefaultWindows(tc.chatType, tc.kind)
		if w.Idle < tc.idleMin || w.Idle > tc.idleMax || w.Max > tc.maxMax {
			t.Fatalf("%s/%s: окна %+v вне спецификации", tc.chatType, tc.kind, w)
		}
	}
}

// TestBufferBatchesHumanBurst — бурст человека (сообщения с паузами ~2 с,
// обычный стиль в Telegram) собирается в ОДИН ход на реальных окнах лички
// (находка приёмки 2026-11-01: прежние 1.5с/3с рвали бурст на отдельные ходы —
// бот отвечал на каждое сообщение по отдельности). Закрытие подменено,
// как в остальных юнит-тестах.
func TestBufferBatchesHumanBurst(t *testing.T) {
	closed := make(chan turnRecord, 8)
	m := NewChatBufferManager(BufferDeps{Pool: nil})
	m.startTurn = func(ctx context.Context, chatID int64, ids []int64) (int64, string, error) {
		closed <- turnRecord{chatID: chatID, ids: ids}
		return 1, "direct", nil
	}
	for i, ev := range []int64{11, 12, 13} {
		if i > 0 {
			time.Sleep(1800 * time.Millisecond) // пауза больше прежних 1.5с idle
		}
		m.Add(PendingEvent{EventID: ev, ChatID: 1, ChatType: ChatPrivate, Kind: KindMessage})
	}
	select {
	case rec := <-closed:
		if len(rec.ids) != 3 {
			t.Fatalf("бурст разорван: первая порция из %d событий %v", len(rec.ids), rec.ids)
		}
	case <-time.After(9 * time.Second):
		t.Fatalf("бурст не закрылся за 9 с")
	}
	select {
	case rec := <-closed:
		t.Fatalf("лишняя порция: %v", rec)
	case <-time.After(500 * time.Millisecond):
	}
	m.Wait()
}

func TestBufferClosesOnIdle(t *testing.T) {
	// Ближайший срок — idle (30 мс), не max (5 с).
	m, closed := newUnitTestManager(t, 30*time.Millisecond, 5*time.Second)
	m.Add(PendingEvent{EventID: 11, ChatID: 1, ChatType: ChatPrivate, Kind: KindMessage})

	rec := waitForRecord(t, closed)
	if rec.chatID != 1 || len(rec.ids) != 1 || rec.ids[0] != 11 {
		t.Fatalf("закрытие: %+v", rec)
	}
	m.Wait()
}

func TestBufferClosesOnMaxDespiteContinuousFlow(t *testing.T) {
	// Поток сообщений быстрее idle — закрытие всё равно наступает по max
	// («поток не переносит закрытие бесконечно», §6.2): 20 событий по 30 мс
	// против idle 80 мс и max 200 мс.
	m, closed := newUnitTestManager(t, 80*time.Millisecond, 200*time.Millisecond)
	for i := 1; i <= 20; i++ {
		m.Add(PendingEvent{EventID: int64(i), ChatID: 2, ChatType: ChatPrivate, Kind: KindMessage})
		time.Sleep(30 * time.Millisecond)
	}

	rec := waitForRecord(t, closed)
	if rec.chatID != 2 || len(rec.ids) == 0 {
		t.Fatalf("закрытие по max: %+v", rec)
	}
	m.Wait()
}

func TestBufferClosesOnEventLimit(t *testing.T) {
	// §6.2: закрыть batch при 20 событиях (здесь лимит ужат до 3).
	m, closed := newUnitTestManager(t, time.Hour, time.Hour) // таймеры не успевают
	m.MaxEvents = 3
	for i := 1; i <= 3; i++ {
		m.Add(PendingEvent{EventID: int64(i), ChatID: 3, ChatType: ChatPrivate, Kind: KindMessage})
	}
	rec := waitForRecord(t, closed)
	if len(rec.ids) != 3 {
		t.Fatalf("лимит событий: %+v", rec.ids)
	}
}

func TestBufferClosesOnTextLimit(t *testing.T) {
	// §6.2: лимит входного текста (символы, паритет .length TS).
	m, closed := newUnitTestManager(t, time.Hour, time.Hour)
	m.MaxTextChars = 5
	long := "по-русски много" // 15 символов
	m.Add(PendingEvent{EventID: 1, ChatID: 4, ChatType: ChatPrivate, Kind: KindMessage, Text: &long})
	rec := waitForRecord(t, closed)
	if len(rec.ids) != 1 {
		t.Fatalf("лимит текста: %+v", rec.ids)
	}
}

func TestBufferFlushAllClosesImmediately(t *testing.T) {
	m, closed := newUnitTestManager(t, time.Hour, time.Hour) // без flush закрытия не будет
	m.Add(PendingEvent{EventID: 9, ChatID: 5, ChatType: ChatPrivate, Kind: KindMessage})

	m.FlushAll(context.Background())
	select {
	case rec := <-closed:
		if rec.chatID != 5 || rec.ids[0] != 9 {
			t.Fatalf("flushAll: %+v", rec)
		}
	default:
		t.Fatalf("flushAll не закрыл буфер")
	}
}

func TestBufferNewBatchAfterClose(t *testing.T) {
	// События после закрытия начинают новую порцию: принадлежность событий
	// уже созданному turn не меняется (§6.2).
	m, closed := newUnitTestManager(t, 20*time.Millisecond, time.Hour)
	m.Add(PendingEvent{EventID: 1, ChatID: 6, ChatType: ChatPrivate, Kind: KindMessage})
	first := waitForRecord(t, closed)

	m.Add(PendingEvent{EventID: 2, ChatID: 6, ChatType: ChatPrivate, Kind: KindMessage})
	second := waitForRecord(t, closed)

	if first.ids[0] != 1 || second.ids[0] != 2 {
		t.Fatalf("порции перемешались: %+v / %+v", first, second)
	}
	m.Wait()
}

// TestBufferCloseRetriesTransientFailure — 29.3: транзиентный сбой createTurn
// (таймаут acquire, рестарт PG) повторяется в живом процессе с backoff, а не
// оставляет события pending до рестарта/recoverStalePending. Подмена сбоя —
// через startTurn: первые две попытки падают, третья создаёт ход.
func TestBufferCloseRetriesTransientFailure(t *testing.T) {
	m, closed := newUnitTestManager(t, 20*time.Millisecond, time.Hour)
	m.RetryDelays = []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}

	var mu sync.Mutex
	attempts := 0
	m.startTurn = func(ctx context.Context, chatID int64, ids []int64) (int64, string, error) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n <= 2 {
			return 0, "", errors.New("сбой БД: connection reset")
		}
		closed <- turnRecord{chatID: chatID, ids: ids}
		return 7, "direct", nil
	}

	m.Add(PendingEvent{EventID: 21, ChatID: 7, ChatType: ChatPrivate, Kind: KindMessage})
	rec := waitForRecord(t, closed)
	if rec.chatID != 7 || len(rec.ids) != 1 || rec.ids[0] != 21 {
		t.Fatalf("закрытие после повторов: %+v", rec)
	}
	m.Wait()

	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Fatalf("createTurn вызван %d раз, хочу 3 (два сбоя + успех)", attempts)
	}
}

// TestBufferCloseGivesUpAfterRetries — если сбой не транзиентный и повторы
// исчерпаны, события честно остаются pending под периодический recovery:
// закрытие не крутится вечно и пишет причину в лог.
func TestBufferCloseGivesUpAfterRetries(t *testing.T) {
	var logs retryLogBuffer
	m := NewChatBufferManager(BufferDeps{Pool: nil, Logger: log.New(&logs, "", 0)})
	m.Windows = func(ChatType, EventKind) Windows { return Windows{Idle: 10 * time.Millisecond, Max: time.Hour} }
	m.RetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	var mu sync.Mutex
	attempts := 0
	m.startTurn = func(context.Context, int64, []int64) (int64, string, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		return 0, "", errors.New("постоянный сбой")
	}

	m.Add(PendingEvent{EventID: 31, ChatID: 8, ChatType: ChatPrivate, Kind: KindMessage})
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		got := attempts
		mu.Unlock()
		if got >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.Wait()
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 3 {
		t.Fatalf("попыток %d, хочу 3 (первая + две паузы)", got)
	}
	if !strings.Contains(logs.String(), "не закрыта после повторов") {
		t.Fatalf("исчерпание повторов не видно в логе:\n%s", logs.String())
	}
}

// retryLogBuffer — потокобезопасный приёмник лога для горутины закрытия.
type retryLogBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *retryLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *retryLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
