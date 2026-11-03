//go:build db

// БД-тесты ретенции фона (блок 18.6): суточная чистка в такте планировщика —
// истёкшие cooldown-ключи консолидации (runtime_state), чекпоинты терминальных
// задач старше 7 дней (task_checkpoints), WAL-потоки bg:* без живой задачи
// старше 30 дней (event_streams). Живое и свежее не трогается; чужие
// (не-фоновые) потоки не удаляются никогда.
package background

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Границы ретенции блока 18.6 (дублируются локально, чтобы тест жил по ТЗ
// независимо от именования констант реализации).
const (
	testCheckpointAge = 7 * 24 * time.Hour
	testWalAge        = 30 * 24 * time.Hour
)

// seedState пишет строку runtime_state (cooldown-ключи консолидации).
func (f *fixture) seedState(t *testing.T, ctx context.Context, key string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("значение runtime_state не сериализуется: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO runtime_state (key, value) VALUES ($1, $2::jsonb)`, key, string(raw)); err != nil {
		t.Fatalf("runtime_state %q не вставлен: %v", key, err)
	}
}

// stateExists — есть ли строка runtime_state.
func (f *fixture) stateExists(t *testing.T, ctx context.Context, key string) bool {
	t.Helper()
	var exists bool
	if err := f.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM runtime_state WHERE key = $1)`, key).Scan(&exists); err != nil {
		t.Fatalf("runtime_state %q не читается: %v", key, err)
	}
	return exists
}

// seedCheckpoint пишет чекпоинт задачи и сдвигает его updated_at в прошлое.
func (f *fixture) seedCheckpoint(t *testing.T, ctx context.Context, unitID string, age time.Duration) {
	t.Helper()
	if err := f.checkpoints.Write(unitID, []byte(`{"attempt":1}`)); err != nil {
		t.Fatalf("чекпоинт %q не записан: %v", unitID, err)
	}
	if age > 0 {
		if _, err := f.pool.Exec(ctx,
			`UPDATE task_checkpoints SET updated_at = now() - make_interval(secs => $1) WHERE task_id = $2`,
			age.Seconds(), unitID); err != nil {
			t.Fatalf("чекпоинт %q не сдвинут в прошлое: %v", unitID, err)
		}
	}
}

// checkpointExists — есть ли чекпоинт задачи.
func (f *fixture) checkpointExists(t *testing.T, ctx context.Context, unitID string) bool {
	t.Helper()
	var exists bool
	if err := f.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM task_checkpoints WHERE task_id = $1)`, unitID).Scan(&exists); err != nil {
		t.Fatalf("чекпоинт %q не читается: %v", unitID, err)
	}
	return exists
}

// seedWalRows пишет n строк WAL-потока (seq 1..n) напрямую со сдвигом created_at.
func (f *fixture) seedWalRows(t *testing.T, ctx context.Context, streamID string, age time.Duration, n int) {
	t.Helper()
	f.seedWalRowsFrom(t, ctx, streamID, age, 1, n)
}

// seedWalRowsFrom — то же с явного seq: несколько порций одного потока (голова
// и хвост с разным возрастом) не должны вставлять один и тот же seq дважды.
func (f *fixture) seedWalRowsFrom(t *testing.T, ctx context.Context, streamID string, age time.Duration, seqFrom, n int) {
	t.Helper()
	for seq := seqFrom; seq < seqFrom+n; seq++ {
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO event_streams (stream_id, seq, type, payload, created_at)
			VALUES ($1, $2, 'task_step_intended', '{}', now() - make_interval(secs => $3))`,
			streamID, seq, age.Seconds()); err != nil {
			t.Fatalf("WAL-строка %q#%d не вставлена: %v", streamID, seq, err)
		}
	}
}

// walCount — сколько строк у потока.
func (f *fixture) walCount(t *testing.T, ctx context.Context, streamID string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM event_streams WHERE stream_id = $1`, streamID).Scan(&n); err != nil {
		t.Fatalf("WAL-поток %q не читается: %v", streamID, err)
	}
	return n
}

// TestRetentionSweep — чистка в такте: истёкший cooldown удалён, живой — нет;
// старый чекпоинт терминальной задачи удалён, живой и свежий — нет; WAL
// мёртвой bg-задачи удалён, живой и чужой поток — нет.
func TestRetentionSweep(t *testing.T) {
	f, ctx := newFixture(t)

	// --- cooldown-ключи консолидации (runtime_state, §18) ---
	seedUntil := func(chat, event int64, until time.Time) string {
		key := fmt.Sprintf("bg:consolidate:cooldown:%d:%d", chat, event)
		f.seedState(t, ctx, key, map[string]string{"until": until.UTC().Format(time.RFC3339)})
		return key
	}
	cooldownExpired := seedUntil(-400100, 1, f.now.Add(-2*consolidationCooldown))
	cooldownLive := seedUntil(-400101, 2, f.now.Add(consolidationCooldown))

	// --- чекпоинты: терминальное намерение (старый — удалить), живое намерение
	// (старый — нет), сведённый и живой эпизоды консолидации, свежий терминал ---
	oldAge := testCheckpointAge + 24*time.Hour
	terminalID := f.createIntention(t, ctx, KindCheckIn, "терминальное намерение ретенции", -6, bgChatID)
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET status = 'completed', updated_at = now() WHERE id = $1`, terminalID); err != nil {
		t.Fatalf("намерение %d не завершено: %v", terminalID, err)
	}
	liveID := f.createIntention(t, ctx, KindCheckIn, "живое намерение ретенции", -6, bgChatID)

	cpTerminalOld := fmt.Sprintf("bg:intention:%d", terminalID)
	f.seedCheckpoint(t, ctx, cpTerminalOld, oldAge)
	cpLiveOld := fmt.Sprintf("bg:intention:%d", liveID)
	f.seedCheckpoint(t, ctx, cpLiveOld, oldAge)

	// Сведённый эпизод: старый чекпоинт его задачи — терминальная работа.
	summChat := int64(-400200)
	summEvent := f.seedEvent(t, ctx, summChat, "private", "сведённый эпизод", f.now.Add(-5*time.Hour))
	if _, err := f.pool.Exec(ctx,
		`UPDATE social_events SET summarized_at = now() WHERE id = $1`, summEvent); err != nil {
		t.Fatalf("событие %d не помечено сведённым: %v", summEvent, err)
	}
	cpConsolDoneOld := fmt.Sprintf("bg:consolidate:%d:%d", summChat, summEvent)
	f.seedCheckpoint(t, ctx, cpConsolDoneOld, oldAge)

	// Живой эпизод консолидации: событие не сведено и является последним в чате.
	openChat := int64(-400201)
	openEvent := f.seedEvent(t, ctx, openChat, "private", "живой эпизод", f.now.Add(-5*time.Hour))
	cpConsolLiveOld := fmt.Sprintf("bg:consolidate:%d:%d", openChat, openEvent)
	f.seedCheckpoint(t, ctx, cpConsolLiveOld, oldAge)

	// Свежий чекпоинт терминальной задачи — недавно отменённое намерение.
	freshTerminal := f.createIntention(t, ctx, KindCheckIn, "свежее терминальное намерение", -6, bgChatID)
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET status = 'cancelled', updated_at = now() WHERE id = $1`, freshTerminal); err != nil {
		t.Fatalf("намерение %d не отменено: %v", freshTerminal, err)
	}
	cpTerminalFresh := fmt.Sprintf("bg:intention:%d", freshTerminal)
	f.seedCheckpoint(t, ctx, cpTerminalFresh, time.Hour)

	// --- WAL-потоки bg:* (event_streams) ---
	walOld := testWalAge + 24*time.Hour
	walDead := fmt.Sprintf("bg:intention:%d", terminalID)
	f.seedWalRows(t, ctx, walDead, walOld, 2)
	walLive := fmt.Sprintf("bg:intention:%d", liveID)
	f.seedWalRows(t, ctx, walLive, walOld, 2)
	walConsolDead := fmt.Sprintf("bg:consolidate:%d:%d", summChat, summEvent)
	f.seedWalRows(t, ctx, walConsolDead, walOld, 2)
	walForeign := "turn:4999" // чужой (не фон) поток — не удаляется никогда
	f.seedWalRows(t, ctx, walForeign, walOld, 1)

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	if f.stateExists(t, ctx, cooldownExpired) {
		t.Fatal("истёкший cooldown-ключ должен удаляться чисткой (18.6)")
	}
	if !f.stateExists(t, ctx, cooldownLive) {
		t.Fatal("живой cooldown-ключ не должен удаляться чисткой (18.6)")
	}

	if f.checkpointExists(t, ctx, cpTerminalOld) {
		t.Fatal("чекпоинт терминальной задачи старше 7 дней должен удаляться (18.6)")
	}
	if !f.checkpointExists(t, ctx, cpLiveOld) {
		t.Fatal("чекпоинт живой задачи не должен удаляться (18.6)")
	}
	if !f.checkpointExists(t, ctx, cpTerminalFresh) {
		t.Fatal("свежий чекпоинт терминальной задачи не должен удаляться (18.6)")
	}
	if f.checkpointExists(t, ctx, cpConsolDoneOld) {
		t.Fatal("чекпоинт сведённого эпизода старше 7 дней должен удаляться (18.6)")
	}
	if !f.checkpointExists(t, ctx, cpConsolLiveOld) {
		t.Fatal("чекпоинт живого эпизода консолидации не должен удаляться (18.6)")
	}

	if n := f.walCount(t, ctx, walDead); n != 0 {
		t.Fatalf("WAL мёртвой bg-задачи должен удаляться (18.6), строк %d", n)
	}
	if n := f.walCount(t, ctx, walLive); n != 2 {
		t.Fatalf("WAL живой задачи не должен удаляться (18.6), строк %d", n)
	}
	if n := f.walCount(t, ctx, walConsolDead); n != 0 {
		t.Fatalf("WAL сведённого эпизода должен удаляться (18.6), строк %d", n)
	}
	if n := f.walCount(t, ctx, walForeign); n != 1 {
		t.Fatalf("чужой (не bg) поток не должен удаляться (18.6), строк %d", n)
	}
}

// TestRetentionDeletesWholeOldStream — minor 11: поток с древней головой и
// свежим хвостом не теряет голову поштучно. Иначе checkContiguous навсегда
// получал ErrSequenceGap, если поток когда-нибудь перечитали.
func TestRetentionDeletesWholeOldStream(t *testing.T) {
	f, ctx := newFixture(t)

	deadID := f.createIntention(t, ctx, KindCheckIn, "мёртвая задача с длинным потоком", -6, bgChatID)
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET status = 'completed', updated_at = now() WHERE id = $1`, deadID); err != nil {
		t.Fatalf("намерение %d не завершено: %v", deadID, err)
	}
	stream := fmt.Sprintf("bg:intention:%d", deadID)
	f.seedWalRowsFrom(t, ctx, stream, testWalAge+24*time.Hour, 1, 1) // голова старше 30 дней
	f.seedWalRowsFrom(t, ctx, stream, time.Hour, 2, 1)               // хвост свежий

	f.sched.Tick(ctx)

	if n := f.walCount(t, ctx, stream); n != 2 {
		t.Fatalf("поток с свежим хвостом удалён частично/целиком: строк %d, хочу 2", n)
	}
}
