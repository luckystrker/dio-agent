//go:build db

// БД-тесты адаптера журнала (требование 9.2): однократность записи, replay
// бит-в-бит, конфликт префикса, обрыв последовательности, запрет DropHistory
// и восстановление потока средствами ago поверх адаптера.
package runtime

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ago/events"
	agostream "ago/stream"
)

// stamp фиксирует время события: replay должен сохранять его без изменений.
var stamp = time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.UTC)

// replayFixtures — репрезентативный отрезок журнала: реплика человека, вызов
// инструмента с вложенными полями, ответ модели с usage, события WAL задачи.
func replayFixtures() []events.Event {
	human := &events.HumanMessage{ParentID: "turn-1", Content: "привет 😎 <тест> & «проверка»"}
	human.CreatedAt = stamp

	call := &events.ToolCallEvent{
		ID:             "call-1",
		Name:           "memory_search",
		Arguments:      `{"query":"кино","limit":3}`,
		VendorMetadata: map[string]any{"provider": "opencode"},
	}
	call.CreatedAt = stamp

	resp := &events.ModelResponse{
		Message: &events.ModelMessage{Content: "вот что я помню о кино"},
		Usage:   events.Usage{PromptTokens: events.I64(120), CompletionTokens: events.I64(34)},
		Model:   "test-model",
	}
	resp.CreatedAt = stamp
	resp.Message.CreatedAt = stamp

	intended := &TaskStepIntended{Step: "fetch-source", Kind: "explore_fetch", Params: map[string]any{"url": "https://example.com"}}
	intended.CreatedAt = stamp

	done := &TaskStepDone{Step: "fetch-source", Result: map[string]any{"ok": true, "chars": float64(512)}}
	done.CreatedAt = stamp

	return []events.Event{human, call, resp, intended, done}
}

// msgEvent — реплика-заглушка с фиксированным временем для тестов префикса.
func msgEvent(parent, content string) events.Event {
	ev := &events.HumanMessage{ParentID: parent, Content: content}
	ev.CreatedAt = stamp
	return ev
}

// countEvents считает строки потока в базе напрямую (мимо адаптера).
func countEvents(t *testing.T, storage *PgStreamStorage, streamID string) int {
	t.Helper()
	var n int
	if err := storage.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM event_streams WHERE stream_id = $1`, streamID).Scan(&n); err != nil {
		t.Fatalf("count(*) по потоку %s: %v", streamID, err)
	}
	return n
}

// sameEvents сверяет две последовательности событий бит-в-бит: тип, время и
// канонические байты сериализации.
func sameEvents(want, got []events.Event) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i].EventType() != got[i].EventType() || !want[i].At().Equal(got[i].At()) {
			return false
		}
		wb, err := events.MarshalEvent(want[i])
		if err != nil {
			return false
		}
		gb, err := events.MarshalEvent(got[i])
		if err != nil {
			return false
		}
		if !bytes.Equal(wb, gb) {
			return false
		}
	}
	return true
}

// Однократность записи (§12.1): повтор той же истории не создаёт вторых
// событий — повторный прогон обработки не меняет журнал.
func TestPgStreamStorageSaveOnce(t *testing.T) {
	ctx := context.Background()
	storage := NewPgStreamStorage(ctx, newTestPool(t))

	first := msgEvent("p1", "первое")
	second := msgEvent("p1", "второе")

	id := "s-once"
	for range 2 {
		if err := storage.SetHistory(id, []events.Event{first}); err != nil {
			t.Fatalf("повторная запись того же события не должна падать: %v", err)
		}
	}
	if n := countEvents(t, storage, id); n != 1 {
		t.Fatalf("повтор (stream, seq) создал %d событий, хочу 1 — однократность записи (§12.1)", n)
	}

	if err := storage.SetHistory(id, []events.Event{first, second}); err != nil {
		t.Fatalf("дозапись: %v", err)
	}
	for range 2 {
		if err := storage.SetHistory(id, []events.Event{first, second}); err != nil {
			t.Fatalf("повтор полной истории: %v", err)
		}
	}
	if n := countEvents(t, storage, id); n != 2 {
		t.Fatalf("после повторов %d событий, хочу 2", n)
	}

	// SaveEvent — тот же путь однократности: дозапись в конец.
	extra := msgEvent("p1", "третье")
	if err := storage.SaveEvent(id, extra); err != nil {
		t.Fatalf("SaveEvent: %v", err)
	}
	history, err := storage.GetHistory(id)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(history) != 3 || history[2].EventType() != "HumanMessage" {
		t.Fatalf("SaveEvent не дописал событие в конец: %d событий", len(history))
	}
}

// Переписывание середины и сокращение истории — ошибка, журнал не меняется
// (решение 2026-10-30: SetHistory — без удалений).
func TestPgStreamStorageHistoryConflict(t *testing.T) {
	ctx := context.Background()
	storage := NewPgStreamStorage(ctx, newTestPool(t))

	evs := []events.Event{msgEvent("p", "а"), msgEvent("p", "б"), msgEvent("p", "в")}
	id := "s-conflict"
	if err := storage.SetHistory(id, evs); err != nil {
		t.Fatalf("первая запись: %v", err)
	}

	rewritten := []events.Event{evs[0], msgEvent("p", "НЕ Б"), evs[2]}
	err := storage.SetHistory(id, rewritten)
	if !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("переписывание середины вернуло %v, хочу ErrHistoryConflict", err)
	}
	if n := countEvents(t, storage, id); n != 3 {
		t.Fatalf("журнал изменился после конфликтной записи: %d событий", n)
	}
	history, err := storage.GetHistory(id)
	if err != nil {
		t.Fatalf("GetHistory после конфликта: %v", err)
	}
	if !sameEvents(evs, history) {
		t.Fatal("журнал после конфликтной записи не совпал с исходной историей")
	}

	shortened := []events.Event{evs[0], evs[1]}
	if err := storage.SetHistory(id, shortened); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("сокращение истории вернуло %v, хочу ErrHistoryConflict", err)
	}
	if n := countEvents(t, storage, id); n != 3 {
		t.Fatalf("сокращение удалило события: %d осталось", n)
	}
}

// Replay бит-в-бит (требование 9.2): записанная последовательность перечитывается
// с теми же типами, временем и байтами сериализации. «Перезапуск» — новый
// адаптер поверх того же пула; восстановление есть перечитывание журнала (§12.2).
func TestPgStreamStorageReplayBitForBit(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	storage := NewPgStreamStorage(ctx, pool)

	built := replayFixtures()
	id := "s-replay"
	if err := storage.SetHistory(id, built); err != nil {
		t.Fatalf("запись журнала: %v", err)
	}

	afterRestart := NewPgStreamStorage(ctx, pool)
	history, err := afterRestart.GetHistory(id)
	if err != nil {
		t.Fatalf("перечитывание журнала: %v", err)
	}
	if len(history) != len(built) {
		t.Fatalf("перечитано %d событий, записано %d", len(history), len(built))
	}
	for i := range built {
		if built[i].EventType() != history[i].EventType() {
			t.Errorf("событие %d: тип %q, хочу %q", i, history[i].EventType(), built[i].EventType())
			continue
		}
		if !built[i].At().Equal(history[i].At()) {
			t.Errorf("событие %d (%s): время %v, хочу %v", i, built[i].EventType(), history[i].At(), built[i].At())
		}
		want, err := events.MarshalEvent(built[i])
		if err != nil {
			t.Fatalf("маршалинг записанного события %d: %v", i, err)
		}
		got, err := events.MarshalEvent(history[i])
		if err != nil {
			t.Fatalf("маршалинг перечитанного события %d: %v", i, err)
		}
		if !bytes.Equal(want, got) {
			t.Errorf("событие %d (%s) перечиталось не бит-в-бит:\n записано:  %s\n перечитано: %s",
				i, built[i].EventType(), want, got)
		}
	}
}

// Обрыв последовательности виден при чтении (требование 9.2): поток с
// пропущенным seq не читается молча — восстановление не должно пропускать шаги.
func TestPgStreamStorageSequenceGap(t *testing.T) {
	ctx := context.Background()
	storage := NewPgStreamStorage(ctx, newTestPool(t))

	id := "s-gap"
	// Порча мимо адаптера: 1, 2, 4 — пропущен seq 3.
	for _, seq := range []int{1, 2, 4} {
		if _, err := storage.pool.Exec(ctx,
			`INSERT INTO event_streams (stream_id, seq, type) VALUES ($1, $2, 'turn_started')`,
			id, seq); err != nil {
			t.Fatalf("подготовка порченого потока (seq %d): %v", seq, err)
		}
	}

	_, err := storage.GetHistory(id)
	if !errors.Is(err, ErrSequenceGap) {
		t.Fatalf("обрыв последовательности вернул %v, хочу ErrSequenceGap", err)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("ошибка должна называть пропущенный seq 3: %v", err)
	}
}

// Расхождение колонки type с @type в payload — повреждение, чтение падает.
func TestPgStreamStorageTypeMismatch(t *testing.T) {
	ctx := context.Background()
	storage := NewPgStreamStorage(ctx, newTestPool(t))

	id := "s-mismatch"
	if _, err := storage.pool.Exec(ctx,
		`INSERT INTO event_streams (stream_id, seq, type, payload) VALUES ($1, 1, 'turn_started', '{"@type":"model_output"}')`,
		id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	_, err := storage.GetHistory(id)
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("расхождение типа вернуло %v, хочу ErrTypeMismatch", err)
	}
}

// DropHistory запрещён (решение 2026-10-30), журнал при этом не трогается.
func TestPgStreamStorageDropForbidden(t *testing.T) {
	ctx := context.Background()
	storage := NewPgStreamStorage(ctx, newTestPool(t))

	evs := []events.Event{msgEvent("p", "выжить должно")}
	id := "s-drop"
	if err := storage.SetHistory(id, evs); err != nil {
		t.Fatalf("запись: %v", err)
	}

	if err := storage.DropHistory(id); !errors.Is(err, ErrDropForbidden) {
		t.Fatalf("DropHistory вернул %v, хочу ErrDropForbidden", err)
	}
	if n := countEvents(t, storage, id); n != 1 {
		t.Fatalf("DropHistory удалил события: осталось %d", n)
	}
}

// Восстановление средствами ago (§12.2): поток строится поверх адаптера через
// stream.LoadHistory и продолжает принимать события — дозапись попадает в
// PostgreSQL тем же механизмом History.Append.
func TestPgStreamStorageResumesAgoStream(t *testing.T) {
	ctx := context.Background()
	storage := NewPgStreamStorage(ctx, newTestPool(t))

	id := "s-resume"
	seed := replayFixtures()[:2] // реплика + вызов инструмента
	if err := storage.SetHistory(id, seed); err != nil {
		t.Fatalf("запись журнала: %v", err)
	}

	// «Перезапуск»: поток восстанавливается перечитыванием журнала.
	resumed, err := agostream.LoadHistory(id, storage, seed)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}

	// Продолжение потока: событие уходит в журнал штатной механикой ago.
	next := msgEvent("turn-1", "и ещё одно после рестарта")
	if err := resumed.Send(next); err != nil {
		t.Fatalf("Send после восстановления: %v", err)
	}

	history, err := storage.GetHistory(id)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	want := append(seed, next)
	if !sameEvents(want, history) {
		t.Fatalf("после восстановления поток не совпал с ожидаемым: %d событий, хочу %d",
			len(history), len(want))
	}
}
