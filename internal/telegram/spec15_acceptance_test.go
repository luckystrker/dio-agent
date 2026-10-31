// Приёмочные тесты этапа 15.2 (тестировщик; §6.2, решение владельца
// 2026-11-01, CHECKLIST.md «Этап 15.2»). Окна пачки лички 1.5с/3с подняты до
// idle 3с / максимум 10с от первого события: сообщения с паузами в пару
// секунд — обычный стиль Telegram — обязаны собираться в ОДИН ход.
// Reply/mention/command (150мс/1с) и пассивная группа (4с/6с) не менялись.
// Написаны по спеке и чек-листу; internal/telegram/buffers.go не читался —
// API подмены (startTurn, Windows) взят из существующих юнит-тестов пакета.
package telegram

import (
	"context"
	"testing"
	"time"
)

// sp15Manager — менеджер с реальными DefaultWindows и подменённым закрытием.
func sp15Manager(t *testing.T, closed chan turnRecord) *ChatBufferManager {
	t.Helper()
	m := NewChatBufferManager(BufferDeps{Pool: nil})
	m.startTurn = func(ctx context.Context, chatID int64, ids []int64) (int64, string, error) {
		closed <- turnRecord{chatID: chatID, ids: ids}
		return 1, "direct", nil
	}
	return m
}

func sp15WaitRecord(t *testing.T, closed chan turnRecord, within time.Duration) turnRecord {
	t.Helper()
	select {
	case rec := <-closed:
		return rec
	case <-time.After(within):
		t.Fatalf("буфер не закрылся за %v", within)
		return turnRecord{}
	}
}

// Точные значения §6.2: личка 3с/10с (решение 2026-11-01), пассивная группа
// 4с/6с (чек-лист: «не менялись»; спека допускает idle 3–5с),
// reply/mention/command 150мс/1с. Максимум — ровно из таблицы; idle —
// значение из чек-листа. Плюс дефолт закрытия при 20 событиях (§6.2).
func TestSpec15DefaultWindowsExactValues(t *testing.T) {
	cases := []struct {
		name     string
		chatType ChatType
		kind     EventKind
		idle     time.Duration
		max      time.Duration
	}{
		{"личка", ChatPrivate, KindMessage, 3 * time.Second, 10 * time.Second},
		{"пассивная группа", ChatGroup, KindMessage, 4 * time.Second, 6 * time.Second},
		{"реплай в группе", ChatGroup, KindReply, 150 * time.Millisecond, time.Second},
		{"mention в группе", ChatGroup, KindMention, 150 * time.Millisecond, time.Second},
		{"команда в группе", ChatGroup, KindCommand, 150 * time.Millisecond, time.Second},
	}
	for _, tc := range cases {
		w := DefaultWindows(tc.chatType, tc.kind)
		if w.Max != tc.max {
			t.Errorf("%s: max = %v, по §6.2 ровно %v", tc.name, w.Max, tc.max)
		}
		if w.Idle != tc.idle {
			t.Errorf("%s: idle = %v, по чек-листу этапа 15.2 («не менялись»/новое значение) ровно %v", tc.name, w.Idle, tc.idle)
		}
	}
	m := NewChatBufferManager(BufferDeps{Pool: nil})
	if m.MaxEvents != 20 {
		t.Errorf("дефолт MaxEvents = %d, по §6.2 — закрытие при 20 событиях", m.MaxEvents)
	}
}

// TestSpec15DirectRequestInPrivateWindows — уточнение §6.2 (решение 2026-11-01,
// находка приёмки): быстрые окна reply/mention/command относятся к ГРУППАМ,
// в личке все сообщения прямые и собираются личными окнами — быстрое закрытие
// в личке отрывало бы команду/реплай от дописываемой пачки.
func TestSpec15DirectRequestInPrivateWindows(t *testing.T) {
	for _, kind := range []EventKind{KindReply, KindMention, KindCommand} {
		w := DefaultWindows(ChatPrivate, kind)
		if w.Idle != 3*time.Second || w.Max != 10*time.Second {
			t.Errorf("личка/%s: окна %+v, хочу личные 3с/10с — быстрые окна только для групп", kind, w)
		}
	}
	// А в группе быстрые окна остаются.
	for _, kind := range []EventKind{KindReply, KindMention, KindCommand} {
		w := DefaultWindows(ChatGroup, kind)
		if w.Max != time.Second || w.Idle > 300*time.Millisecond {
			t.Errorf("группа/%s: окна %+v, хочу 0–300мс/1с", kind, w)
		}
	}
}

// Бурст человека у границы нового idle: три сообщения с паузами 2.4с
// (больше прежних 1.5с — прежде разорвалось бы) собираются в ОДИН ход.
func TestSpec15BufferBatchesBurstNearIdleBoundary(t *testing.T) {
	closed := make(chan turnRecord, 8)
	m := sp15Manager(t, closed)

	m.Add(PendingEvent{EventID: 41, ChatID: 7, ChatType: ChatPrivate, Kind: KindMessage})
	time.Sleep(2400 * time.Millisecond)
	m.Add(PendingEvent{EventID: 42, ChatID: 7, ChatType: ChatPrivate, Kind: KindMessage})
	time.Sleep(2400 * time.Millisecond)
	m.Add(PendingEvent{EventID: 43, ChatID: 7, ChatType: ChatPrivate, Kind: KindMessage})

	rec := sp15WaitRecord(t, closed, 10*time.Second) // закрытие ≈ 7.8с от старта
	if len(rec.ids) != 3 {
		t.Fatalf("бурст у границы idle разорван: первая порция %v", rec.ids)
	}
	select {
	case rec := <-closed:
		t.Fatalf("лишняя порция: %v", rec)
	case <-time.After(600 * time.Millisecond):
	}
	m.Wait()
}

// Обратная сторона: пауза БОЛЬШЕ idle (3.6с > 3с) рвёт пачку — окно реально
// работает, а не заменено бесконечным ожиданием (§6.2: idle закрывает порцию).
func TestSpec15BufferSplitsBurstWhenPauseExceedsIdle(t *testing.T) {
	closed := make(chan turnRecord, 8)
	m := sp15Manager(t, closed)

	m.Add(PendingEvent{EventID: 51, ChatID: 8, ChatType: ChatPrivate, Kind: KindMessage})
	time.Sleep(3600 * time.Millisecond)
	m.Add(PendingEvent{EventID: 52, ChatID: 8, ChatType: ChatPrivate, Kind: KindMessage})

	first := sp15WaitRecord(t, closed, 6*time.Second)
	if len(first.ids) != 1 || first.ids[0] != 51 {
		t.Fatalf("первая порция: %+v, хочу только событие 51", first.ids)
	}
	second := sp15WaitRecord(t, closed, 9*time.Second)
	if len(second.ids) != 1 || second.ids[0] != 52 {
		t.Fatalf("вторая порция: %+v, хочу только событие 52", second.ids)
	}
	m.Wait()
}

// Максимум — 10с от ПЕРВОГО события, непрерывный поток с интервалом меньше
// idle не переносит закрытие бесконечно (§6.2): события каждые 2.5с, порция
// обязана закрыться около t=10с (не раньше — не по idle), остаток — новой
// порцией.
func TestSpec15BufferMaxDeadlineFromFirstEventRealWindows(t *testing.T) {
	closed := make(chan turnRecord, 8)
	m := sp15Manager(t, closed)

	start := time.Now()
	go func() {
		for i, id := range []int64{61, 62, 63, 64, 65, 66} {
			if i > 0 {
				time.Sleep(2500 * time.Millisecond)
			}
			m.Add(PendingEvent{EventID: id, ChatID: 9, ChatType: ChatPrivate, Kind: KindMessage})
		}
	}()

	// Первая порция: по max-дедлайну (~10с), не раньше.
	rec := sp15WaitRecord(t, closed, 13*time.Second)
	elapsed := time.Since(start)
	if elapsed < 9500*time.Millisecond {
		t.Fatalf("первая порция закрылась через %v — раньше max 10с (значит, по idle: поток разорван)", elapsed)
	}
	if len(rec.ids) < 4 {
		t.Fatalf("к max-дедлайну накопилось только %v (хочу минимум события 61–64)", rec.ids)
	}
	// Остаток потока — новой порцией (события после закрытия начинают её, §6.2).
	rest := sp15WaitRecord(t, closed, 10*time.Second)
	all := append(append([]int64{}, rec.ids...), rest.ids...)
	if len(all) != 6 {
		t.Fatalf("потеряны события потока: %v + %v", rec.ids, rest.ids)
	}
	seen := map[int64]bool{}
	for _, id := range all {
		if seen[id] {
			t.Fatalf("событие %d попало в две порции: %v / %v", id, rec.ids, rest.ids)
		}
		seen[id] = true
	}
	m.Wait()
}
