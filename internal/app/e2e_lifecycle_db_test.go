//go:build db

// E2E жизненного цикла процесса: singleton §5.1 (второй процесс выходит с
// ошибкой), graceful shutdown §5.3 (остановил приём → дождался текущего →
// закрыл соединения) и recovery без graceful (§5.3: «recovery обязан работать
// и после SIGKILL»): pending-события без turn и зависший sending из БД.
package app

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/internal/telegram"
	"github.com/luckystrker/dio-agent/migrations"
)

const lcChat = int64(200701)

const lcAnswer = `{"decision":"answer","messages":["жизненный цикл в порядке"],"reason":"ответ"}`

// bareOptions — минимальная сборка без модели и Telegram: для проверки
// singleton и recovery без содержательной работы.
func bareOptions(dsn string) Options {
	return Options{
		Config:         runtime.Config{DatabaseURL: dsn, NodeEnv: runtime.NodeEnvTest},
		Logger:         log.New(io.Discard, "", 0),
		SingletonWatch: 20 * time.Millisecond,
	}
}

// TestLifecycleSingletonBusy — §5.1: замок занят — второй процесс не
// запускается, ошибка явная (код выхода 1, restart systemd не спасёт).
func TestLifecycleSingletonBusy(t *testing.T) {
	dsn := newTestDB(t)
	newHarnessDSN(t, dsn) // первый процесс держит замок

	b, err := New(bareOptions(dsn))
	if err != nil {
		t.Fatalf("второе приложение не собралось: %v", err)
	}
	err = b.Start(context.Background())
	if !errors.Is(err, runtime.ErrSingletonTaken) {
		t.Fatalf("второй процесс запустился или упал с чужой ошибкой: %v", err)
	}
}

// TestLifecycleGracefulShutdown — §5.3: остановить приём → ограниченно
// дождаться текущего хода → закрыть соединения; замок освобождён —
// следующий запуск возможен немедленно.
func TestLifecycleGracefulShutdown(t *testing.T) {
	dsn := newTestDB(t)
	block := &blockingEmbedder{release: make(chan struct{})}
	h := newHarnessDSNTuned(t, dsn, func(o *Options) { o.Embedder = block },
		toolResp("c-g1", "memory_search", `{"query":"грацело"}`),
		textResp(lcAnswer),
	)

	h.api.push(1, lcChat, "private", "проверка graceful shutdown")
	turnID := waitForTurn(t, h, lcChat)
	waitJournalEvent(h, "ход ушёл в генерацию", turnID, "tool_call")

	// Shutdown, пока текущий ход висит на внешнем вызове.
	stopped := make(chan struct{})
	go func() {
		h.app.Stop(context.Background())
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatalf("Stop вернулся, не дождавшись текущего хода")
	case <-time.After(100 * time.Millisecond):
		// Stop ещё ждёт — как и должно быть (§5.3 «ограниченно дождаться»).
	}

	// Внешний вызов завершился — текущий ход дорабатывает, и Stop возвращается
	// СРАЗУ после него, а не упирается в DrainLimit (§5.3 «ограниченно
	// дождаться текущих»); допуск заведомо меньше лимита (10 с харнесса).
	start := time.Now()
	close(block.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatalf("Stop не вернулся в течение 2 с после завершения текущего хода — worker не получил drain (лимит %v)", h.app.opts.drainLimit())
	}
	if elapsed := time.Since(start); elapsed >= h.app.opts.drainLimit() {
		t.Fatalf("Stop упёрся в лимит %v вместо возврата за %v после завершения хода (§5.3)", h.app.opts.drainLimit(), elapsed)
	}

	if status := queryString(h, `SELECT status FROM turns WHERE id = $1`, turnID); status != "applied" {
		t.Fatalf("текущий ход не применён при graceful shutdown: %s", status)
	}

	// Соединения закрыты, замок свободен: следующий запуск возможен сразу.
	// bareOptions не задаёт DrainLimit — намеренно дефолтные 90 с: холостой
	// Stop обязан вернуться быстро, а не ждать весь лимит (§5.3).
	b, err := New(bareOptions(dsn))
	if err != nil {
		t.Fatalf("второе приложение не собралось: %v", err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("singleton не освобождён при shutdown: %v", err)
	}
	idleStopStart := time.Now()
	b.Stop(context.Background())
	if elapsed := time.Since(idleStopStart); elapsed >= DefaultDrainLimit {
		t.Fatalf("холостой Stop ждал весь лимит %v (реально %v) — drain worker не подан (§5.3)", DefaultDrainLimit, elapsed)
	}
}

// TestLifecycleRecoverPendingWithoutGrace — §12.2 «событие pending, turn_started
// нет → восстановить буфер»: процесс убит после ingest, до закрытия порции;
// перезапуск собирает pending-события в ход и доводит его.
func TestLifecycleRecoverPendingWithoutGrace(t *testing.T) {
	dsn := newTestDB(t)
	// Процесс A: окна буфера на порядок больше теста — порция не закроется.
	hA := newHarnessDSNTuned(t, dsn, func(o *Options) {
		o.Buffers = func(telegram.ChatType, telegram.EventKind) telegram.Windows {
			return telegram.Windows{Idle: 10 * time.Second, Max: 15 * time.Second}
		}
	}, textResp(lcAnswer))
	hA.api.push(1, lcChat, "private", "сообщение в незакрытом буфере")

	eventually(t, "событие сохранено (pending)", func() bool {
		return queryInt(hA, `SELECT count(*) FROM social_events WHERE status = 'pending'`) > 0
	})
	if queryInt(hA, `SELECT count(*) FROM turns`) != 0 {
		t.Fatalf("ход создан до закрытия порции — окна не работают")
	}
	hA.hardKill() // SIGKILL: таймеры буфера потеряны

	// Перезапуск: recovery собирает pending в ход (принадлежность — в БД, §6.2).
	hB := newHarnessDSN(t, dsn, textResp(lcAnswer))
	eventually(t, "recovery довёл ход до доставки", func() bool {
		return len(hB.api.callsOf("sendMessage")) == 1
	})
	if n := queryInt(hB, `SELECT count(*) FROM social_events WHERE status <> 'processed'`); n != 0 {
		t.Fatalf("событие не закрыто ходом: %d", n)
	}
}

// TestLifecycleRecoverStaleSending — §12.2 «outbox sending старого процесса →
// unknown, не отправлять вслепую»: строка, зависшая в sending, переводится
// в unknown при старте и не отправляется повторно.
func TestLifecycleRecoverStaleSending(t *testing.T) {
	dsn := newTestDB(t)

	// Схема до старта приложения: сеем состояние мёртвого процесса —
	// доставка в sending, updated_at давно; recovery увидит её при старте.
	pool := poolFor(t, dsn)
	if _, err := runtime.Migrate(context.Background(), pool, migrations.FS); err != nil {
		t.Fatalf("схема не применена: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, attempts, updated_at)
		VALUES (1, 0, $1, '{"kind":"message","text":"зависшая доставка"}'::jsonb, 'sending', 1,
		        now() - interval '10 minutes')`, lcChat); err != nil {
		t.Fatalf("зависшая доставка не посеяна: %v", err)
	}

	h := newHarnessDSNTuned(t, dsn, func(o *Options) { o.Telegram = newStubAPI() })

	eventually(t, "зависшая sending переведена в unknown", func() bool {
		st, ok := queryStringOK(h, `SELECT status FROM delivery_outbox WHERE turn_id = 1`)
		return ok && st == "unknown"
	})

	// Не отправляется вслепую: такты доставки не трогают unknown.
	time.Sleep(200 * time.Millisecond)
	if n := len(h.api.callsOf("sendMessage")); n != 0 {
		t.Fatalf("unknown отправлена вслепую: %d вызовов", n)
	}
}
