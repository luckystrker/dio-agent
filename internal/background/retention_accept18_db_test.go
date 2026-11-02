//go:build db

// Дополнительные БД-проверки приёмки блока 18.6 (тестировщик): сценарии, не
// покрытые тестами разработчика, — инвариант append-only журнала после
// пересоздания триггера, WAL-поток эпизода, ушедшего вперёд (главный случай
// ТЗ), чужие ключи runtime_state и суточность чистки.
package background

import (
	"fmt"
	"testing"
	"time"
)

// TestEventStreamsAppendOnlyAfterRetentionMigration — миграция 000013
// пересоздаёт триггер append-only (§12.1, инвариант 13): после неё UPDATE
// любой строки и DELETE всего, кроме старых bg-потоков, по-прежнему
// запрещены; исключение работает только для bg:* старше 30 дней.
func TestEventStreamsAppendOnlyAfterRetentionMigration(t *testing.T) {
	f, ctx := newFixture(t)

	freshBg := "bg:intention:900001"
	oldBg := "bg:intention:900002"
	oldForeign := "turn:900003"
	f.seedWalRows(t, ctx, freshBg, 0, 1)
	f.seedWalRows(t, ctx, oldBg, testWalAge+24*time.Hour, 1)
	f.seedWalRows(t, ctx, oldForeign, testWalAge+24*time.Hour, 1)

	expectForbidden := func(name string, sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err == nil {
			t.Fatalf("%s обязан запрещаться триггером append-only (§12.1) после миграции 000013", name)
		}
	}

	expectForbidden("UPDATE строки журнала",
		`UPDATE event_streams SET payload = '{}' WHERE stream_id = $1`, oldForeign)
	expectForbidden("DELETE свежего bg-потока",
		`DELETE FROM event_streams WHERE stream_id = $1`, freshBg)
	expectForbidden("DELETE старого чужого потока",
		`DELETE FROM event_streams WHERE stream_id = $1`, oldForeign)

	// Единственное исключение: старый bg-поток (ретенционный вывоз 18.6).
	if _, err := f.pool.Exec(ctx,
		`DELETE FROM event_streams WHERE stream_id = $1`, oldBg); err != nil {
		t.Fatalf("DELETE старого bg-потока обязан проходить (исключение 18.6): %v", err)
	}
	if n := f.walCount(t, ctx, oldBg); n != 0 {
		t.Fatalf("старый bg-поток обязан удалиться напрямую, строк %d", n)
	}
}

// TestRetentionMovedForwardEpisode — главный случай ТЗ 18.6 для WAL: у
// растущего эпизода меняется lastEventID → новый unitID; старый поток уже
// никогда не читается и обязан вывозиться, пока событие ещё не сведено.
// Поток текущего (последнего, несведённого) события остаётся.
func TestRetentionMovedForwardEpisode(t *testing.T) {
	f, ctx := newFixture(t)

	chat := int64(-400300)
	e1 := f.seedEvent(t, ctx, chat, "private", "первое событие эпизода", f.now.Add(-10*time.Hour))
	e2 := f.seedEvent(t, ctx, chat, "private", "второе событие эпизода", f.now.Add(-5*time.Hour))

	old := testWalAge + 24*time.Hour
	abandoned := fmt.Sprintf("bg:consolidate:%d:%d", chat, e1)
	current := fmt.Sprintf("bg:consolidate:%d:%d", chat, e2)
	f.seedWalRows(t, ctx, abandoned, old, 2)
	f.seedWalRows(t, ctx, current, old, 2)

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	if n := f.walCount(t, ctx, abandoned); n != 0 {
		t.Fatalf("WAL-поток брошенного unitID (эпизод ушёл вперёд) обязан вывозиться (18.6), строк %d", n)
	}
	if n := f.walCount(t, ctx, current); n != 2 {
		t.Fatalf("WAL-поток живого эпизода (последнее несведённое событие) не должен удаляться (18.6), строк %d", n)
	}
}

// TestRetentionSweepCadenceAndForeignState — чистка суточна (не каждый такт),
// чужие ключи runtime_state не трогает, битый cooldown-ключ (без until) вывозит.
func TestRetentionSweepCadenceAndForeignState(t *testing.T) {
	f, ctx := newFixture(t)

	// Чужой ключ runtime_state (poll offset §18) обязан переживать чистку.
	f.seedState(t, ctx, "telegram:poll_offset", map[string]any{"offset": 42})
	// Битый cooldown: срока не разобрать — вывоз (консолидацию он не блокирует).
	broken := "bg:consolidate:cooldown:-400400:7"
	f.seedState(t, ctx, broken, map[string]string{"oops": "срока нет"})
	// Истёкший cooldown до первого такта — вывозится сразу.
	expired1 := "bg:consolidate:cooldown:-400401:8"
	f.seedState(t, ctx, expired1, map[string]string{
		"until": f.now.Add(-48 * time.Hour).UTC().Format(time.RFC3339)})

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}
	if !f.stateExists(t, ctx, "telegram:poll_offset") {
		t.Fatal("чужой ключ runtime_state не должен удаляться чисткой (18.6)")
	}
	if f.stateExists(t, ctx, broken) {
		t.Fatal("битый cooldown-ключ (без until) должен удаляться чисткой (18.6)")
	}
	if f.stateExists(t, ctx, expired1) {
		t.Fatal("истёкший cooldown-ключ должен удаляться чисткой (18.6)")
	}

	// Новый истёкший cooldown тем же днём: чистка суточна — ключ переживёт
	// второй такт и уедет только после истечения суток.
	expired2 := "bg:consolidate:cooldown:-400402:9"
	f.seedState(t, ctx, expired2, map[string]string{
		"until": f.now.Add(-48 * time.Hour).UTC().Format(time.RFC3339)})
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("второй такт не удался: %v", err)
	}
	if !f.stateExists(t, ctx, expired2) {
		t.Fatal("чистка суточна: повторный прогон тем же днём не должен вывозить новые ключи (18.6)")
	}

	// Сутки прошли — следующий такт вывозит.
	f.setNow(f.now.Add(25 * time.Hour))
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("третий такт не удался: %v", err)
	}
	if f.stateExists(t, ctx, expired2) {
		t.Fatal("после истечения суток чистка обязана вывозить истёкший cooldown (18.6)")
	}
}
