//go:build db

// Дополнительные БД-тесты изоляции паник фона (пункт 16.2, приёмка): (б) паника
// консолидации одного эпизода — ошибка ЭТОЙ задачи, соседний эпизод того же
// такта всё равно исполняется, эпизод не помечается и сводится повторным
// тактом; (а) цикл Run не только ловит первую панику такта, но и продолжает
// тикать дальше.
package background

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

// TestTickIsolatesConsolidationPanic — 16.2(б): в такте два готовых эпизода,
// консолидация паникует на каждом (nil-WAL: s.wal.Steps на nil-приёмнике) —
// такт завершается без ошибки, ОБА эпизода попытаны (изоляция каждого), ни
// один не помечен summarized (повтор следующим тактом, §10.2/§12.2), и после
// починки WAL оба сводятся штатно — первая паника не отравила задачи.
func TestTickIsolatesConsolidationPanic(t *testing.T) {
	f, ctx := newFixture(t)
	// Модель нужна до первого такта: без неё Tick выходит до эпизодов.
	// Ответ не расходуется — паника в консолидации раньше модельного вызова.
	f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: consolidationJSON(
			"Не будет вызван: паника раньше модели.", "не важно")}})
	otherChat := int64(-200900)
	idsA, _ := f.seedEpisode(t, ctx, bgChatID, "private", 3, f.now.Add(-5*time.Hour))
	idsB, _ := f.seedEpisode(t, ctx, otherChat, "private", 3, f.now.Add(-5*time.Hour))

	var buf syncBuffer
	f.sched.logger = log.New(&buf, "", 0)
	f.sched.wal = nil // паника внутри runConsolidation (после startTask)

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("паника одного эпизода не должна валить такт: %v", err)
	}
	if got := strings.Count(buf.String(), "задача изолирована"); got != 2 {
		t.Fatalf("оба эпизода такта должны быть попытаны, каждый со своей изоляцией паники; изоляций %d, лог: %q", got, buf.String())
	}
	for name, ids := range map[string][]int64{"основной чат": idsA, "соседний чат": idsB} {
		var unsummarized int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NULL`, ids).
			Scan(&unsummarized); err != nil {
			t.Fatalf("события (%s) не читаются: %v", name, err)
		}
		if unsummarized != len(ids) {
			t.Fatalf("паниковавшая консолидация не должна помечать события (%s): не помечено %d из %d", name, unsummarized, len(ids))
		}
	}

	// Повтор с починенным WAL: эпизоды не отравлены первой попыткой — сводятся.
	f.sched.wal = f.wal
	f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: consolidationJSON(
			"Эпизод А сведён после паники.", "тема спокойно закрыта")}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: consolidationJSON(
			"Эпизод Б сведён после паники.", "тема спокойно закрыта")}},
	)
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("повторный такт после паники не удался: %v", err)
	}
	var left int
	all := append(append([]int64{}, idsA...), idsB...)
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NULL`, all).
		Scan(&left); err != nil {
		t.Fatalf("события не читаются: %v", err)
	}
	if left != 0 {
		t.Fatalf("после повторного такта оба эпизода должны быть сведены, не помечено %d", left)
	}
}

// TestRunContinuesAfterTickPanic — 16.2(а): цикл Run обязан не просто поймать
// первую панику такта, а продолжать тикать — каждая паника такта (nil-часы:
// dueEpisodes вызывает s.now) должна давать свою запись в логе.
func TestRunContinuesAfterTickPanic(t *testing.T) {
	f, ctx := newFixture(t)
	f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: exploreTurnJSON}})
	var buf syncBuffer
	f.sched.logger = log.New(&buf, "", 0)
	f.sched.opts.MinInterval = time.Millisecond
	f.sched.opts.MaxInterval = time.Millisecond
	f.sched.now = nil // каждый такт паникует в dueEpisodes

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = f.sched.Run(runCtx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(buf.String(), "паника такта") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("цикл Run должен пережить панику такта и тикать дальше (нужно ≥2 паники такта), лог: %q", buf.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не остановился после отмены")
	}
	if !strings.Contains(buf.String(), "ERROR") {
		t.Fatalf("паника такта должна писаться уровнем ERROR, лог: %q", buf.String())
	}
}
