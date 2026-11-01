//go:build db

// БД-тесты изоляции паник фона (пункт 16.2): паника одной фоновой задачи
// обрабатывается как ошибка этой задачи и не мешает остальным в такте;
// паника самого такта пишется в лог ERROR и не роняет цикл Run (а с ним весь
// процесс с приёмом и доставкой). Инъекция паники — nil-журнал (инициатива
// падает на записи turn_started) и nil-часы (dueEpisodes зовёт s.now).
// Запуск против Postgres из docker-compose: см. testpool_db_test.go.
package background

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

// TestTickIsolatesTaskPanic — паника первого намерения такта (инициатива на
// nil-журнале) становится ошибкой ЭТОЙ задачи: намерение возвращается в
// pending с паузой повтора (§12.1), а explore того же такта исполняется.
// До фикса паника валила весь такт и процесс.
func TestTickIsolatesTaskPanic(t *testing.T) {
	f, ctx := newFixture(t)
	f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: exploreTurnJSON}},
	)
	f.seedPrivateChat(t, ctx, "давно не общались", f.now.Add(-6*time.Hour))
	// Инициатива создаётся первой: claim идёт по not_before, id — она первой
	// и исполняется, её паника не должна задеть следующий за ней explore.
	initiative := f.createIntention(t, ctx, KindReplyLater, "тема с паникой", -6, bgChatID)
	explore := f.createIntention(t, ctx, KindExplore, "тема спокойная", -6, bgChatID)

	// nil-журнал: инициатива паникует на WriteTurnStarted (до вызова модели);
	// explore журнал не трогает и должен пройти целиком.
	f.sched.journal = nil

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("паника одной задачи не должна валить такт: %v", err)
	}
	if st := f.intentionStatus(t, ctx, explore); st != "completed" {
		t.Fatalf("explore должен исполниться, несмотря на панику соседа, статус %s", st)
	}
	if st := f.intentionStatus(t, ctx, initiative); st != "pending" {
		t.Fatalf("паниковавшее намерение должно вернуться в pending (§12.1 retry_wait), статус %s", st)
	}
	if nb := f.intentionNotBefore(t, ctx, initiative); !nb.After(f.now) {
		t.Fatalf("повтор паниковавшего намерения должен ждать паузу, not_before=%v (сейчас %v)", nb, f.now)
	}
}

// TestRunSurvivesTickPanic — паника самого такта (nil-часы: dueEpisodes
// вызывает s.now) пишется в лог ERROR, цикл Run продолжает тикать и чисто
// останавливается отменой. До фикса паника в горутине Run убивала процесс.
func TestRunSurvivesTickPanic(t *testing.T) {
	f, ctx := newFixture(t)
	f.withModel(t,
		&events.ModelResponse{Message: &events.ModelMessage{Content: exploreTurnJSON}},
	)
	var buf syncBuffer
	f.sched.logger = log.New(&buf, "", 0)
	f.sched.opts.MinInterval = time.Millisecond
	f.sched.opts.MaxInterval = time.Millisecond
	f.sched.now = nil // паника такта: dueEpisodes зовёт s.now()

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = f.sched.Run(runCtx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "паника") {
		if time.Now().After(deadline) {
			t.Fatalf("паника такта не залогирована, лог: %q", buf.String())
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
