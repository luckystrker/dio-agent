// Юнит-тесты буферов §6.2: окна по типам чатов, ближайший срок таймера,
// закрытие по лимитам, max не переносится бесконечно, flushAll. Закрытие
// подменено фейком (логика окон без БД); транзакционное закрытие — в БД-тестах.
package telegram

import (
	"context"
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
	// §6.2: личка 1–2с/3с; пассивная группа 3–5с/6с; reply/mention/command 0–300мс/1с.
	cases := []struct {
		chatType ChatType
		kind     EventKind
		idleMin  time.Duration
		idleMax  time.Duration
		maxMax   time.Duration
	}{
		{ChatPrivate, KindMessage, time.Second, 2 * time.Second, 3 * time.Second},
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
