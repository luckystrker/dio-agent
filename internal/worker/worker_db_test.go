//go:build db

// БД-тесты когнитивного worker (§5.2, §12.2): приоритет прямого обращения
// над группой, порядок внутри чата, ограниченный повтор с паузой после
// транзитного сбоя и терминальный пропуск после исчерпания попыток.
// Запуск:
//
//	go test -tags db ./internal/worker/
//
// против Postgres из docker-compose; TEST_DATABASE_URL переопределяет DSN
// (на машине разработчика docker-compose занимает 5433 — см. 9.1).
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/migrations"
)

const defaultTestDSN = "postgres://dio:dio@localhost:5432/dio?sslmode=disable"

// fakeRunner — двойной исполнитель хода: пишет порядок вызовов, умеет падать
// первые N раз.
type fakeRunner struct {
	pool    *runtime.Pool
	mu      sync.Mutex
	order   []int64
	fails   map[int64]int // turn_id → сколько раз упасть
	blocked chan struct{} // не nil — блокировать вызовы до закрытия
}

func (r *fakeRunner) Run(ctx context.Context, in character.Input) (character.Outcome, error) {
	r.mu.Lock()
	r.order = append(r.order, in.TurnID)
	fails := r.fails[in.TurnID]
	if fails > 0 {
		r.fails[in.TurnID] = fails - 1
	}
	blocked := r.blocked
	r.mu.Unlock()
	if blocked != nil {
		select {
		case <-ctx.Done():
			return character.Outcome{}, ctx.Err()
		case <-blocked:
		}
	}
	if fails > 0 {
		return character.Outcome{}, errors.New("транзитный сбой двойного исполнителя")
	}
	// Продакшн-Runner применяет ход CAS-ом turns.status='applied' (§12.1);
	// двойной исполнитель эмулирует это, иначе ход остаётся created и worker
	// честно берёт его снова.
	if _, err := r.pool.Exec(ctx, `UPDATE turns SET status = 'applied' WHERE id = $1`, in.TurnID); err != nil {
		return character.Outcome{}, err
	}
	return character.Outcome{}, nil
}

func (r *fakeRunner) called() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.order...)
}

// seedTurn — ход в очереди с одним событием (чат, тип, текст).
func seedTurn(t *testing.T, pool *runtime.Pool, chatID int64, chatType, text string) int64 {
	t.Helper()
	ctx := context.Background()
	var turnID int64
	if err := pool.QueryRow(ctx, `INSERT INTO turns (chat_id) VALUES ($1) RETURNING id`, chatID).Scan(&turnID); err != nil {
		t.Fatalf("ход не создан: %v", err)
	}
	raw := fmt.Sprintf(`{"message":{"message_id":%d,"text":%q,"from":{"id":777}}}`, 4000+turnID, text)
	if _, err := pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, turn_id)
		VALUES ($1, $2, $3, 777, 'Коля', 'message', $4, $5::jsonb, 'buffered', $6)`,
		9_000_000+turnID, chatID, chatType, text, raw, turnID); err != nil {
		t.Fatalf("событие не вставлено: %v", err)
	}
	return turnID
}

// newWorkerPool — чистая тестовая база с полной схемой §18
// (копия помощника 9.2–9.6: пакеты не зависят от внутренностей чужих тестов).
func newWorkerPool(t *testing.T) *runtime.Pool {
	t.Helper()
	ctx := context.Background()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL недоступен по TEST_DATABASE_URL=%q — БД-тест пропущен: %v", dsn, err)
	}
	defer admin.Close(ctx)

	dbName := fmt.Sprintf("dio_worker_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("тестовая база не создана: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)")
	})

	testDSN, err := swapWorkerDSN(dsn, dbName)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL не разобран как URL: %v", err)
	}
	pool, err := runtime.NewPool(ctx, testDSN)
	if err != nil {
		t.Fatalf("пул к тестовой базе не создан: %v", err)
	}
	t.Cleanup(pool.Close)

	result, err := runtime.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatalf("схема не применена: %v", err)
	}
	if result.Applied != 14 || result.Skipped != 0 {
		t.Fatalf("миграции: applied=%d skipped=%d, хочу 14/0", result.Applied, result.Skipped)
	}
	return pool
}

func swapWorkerDSN(dsn, dbName string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, dbName), nil
}

// TestWorkerPrioritizesDirect — §5.2 приоритет 1: более поздний ход личного
// чата обрабатывается раньше более раннего группового.
func TestWorkerPrioritizesDirect(t *testing.T) {
	pool := newWorkerPool(t)

	// Групповой ход СТАРШЕ по id: без приоритета worker взял бы его первым.
	groupTurn := seedTurn(t, pool, -3001, "group", "шум группы")
	directTurn := seedTurn(t, pool, 3001, "private", "прямой вопрос")

	runner := &fakeRunner{pool: pool}
	w := New(Deps{Pool: pool, Runner: runner, Logger: log.New(discardWriter{}, "", 0)},
		Options{PollInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	eventually(t, "оба хода обработаны", func() bool {
		return len(runner.called()) == 2
	})
	w.StopDrain()
	<-w.Done()

	order := runner.called()
	if order[0] != directTurn || order[1] != groupTurn {
		t.Fatalf("порядок нарушен: хочу [прямой %d, группа %d], получено %v", directTurn, groupTurn, order)
	}
	if !orderEqual(order, []int64{directTurn, groupTurn}) {
		t.Fatalf("лишние обработки: %v", order)
	}
}

// TestWorkerKeepsChatOrderWhileHeadPaused — M2: ход на паузе повтора держит
// очередь своего чата: более поздний ход того же чата не обгоняет его.
func TestWorkerKeepsChatOrderWhileHeadPaused(t *testing.T) {
	pool := newWorkerPool(t)
	ctx := context.Background()

	first := seedTurn(t, pool, 4201, "private", "ранний вопрос")
	second := seedTurn(t, pool, 4201, "private", "поздний вопрос")
	other := seedTurn(t, pool, 4202, "private", "другой чат")

	w := New(Deps{Pool: pool}, Options{PollInterval: time.Millisecond})
	w.notBefore[first] = time.Now().Add(time.Hour)

	got, err := w.claimNext(ctx)
	if err != nil {
		t.Fatalf("claimNext: %v", err)
	}
	if got == nil || got.ID != other {
		t.Fatalf("взят ход %v, хочу ход другого чата %d (поздний ход того же чата обгонять не должен)", got, other)
	}

	// Пауза снята — ранний ход снова голова очереди.
	delete(w.notBefore, first)
	got, err = w.claimNext(ctx)
	if err != nil {
		t.Fatalf("claimNext после паузы: %v", err)
	}
	if got == nil || got.ID != first {
		t.Fatalf("после паузы взят %v, хочу ранний ход %d", got, first)
	}
	_ = second
}

// TestWorkerRetriesThenSkips — §12.1/§12.2: транзитный сбой — повтор с паузой;
// исчерпание попыток — терминальный пропуск (turn_skipped, инвариант 5).
func TestWorkerRetriesThenSkips(t *testing.T) {
	pool := newWorkerPool(t)
	ctx := context.Background()

	storage := runtime.NewPgStreamStorage(ctx, pool)
	journal := character.NewTurnJournal(storage, nil)

	turnID := seedTurn(t, pool, 3002, "private", "ход со сбоем исполнителя")

	// Первые 2 попытки падают (лимит 3): третья — успех.
	runner := &fakeRunner{pool: pool, fails: map[int64]int{turnID: 2}}
	w := New(Deps{Pool: pool, Runner: runner, Journal: journal,
		Logger: log.New(discardWriter{}, "", 0)},
		Options{PollInterval: 10 * time.Millisecond, RetryBase: 20 * time.Millisecond, RetryMax: 100 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(runCtx) }()

	eventually(t, "ход выполнен с третьей попытки", func() bool {
		return len(runner.called()) == 3
	})
	w.StopDrain()
	<-w.Done()

	// Пауза между попытками была: первые две попытки не подряд (retry_wait §12.1).
	if !orderEqual(runner.called(), []int64{turnID, turnID, turnID}) {
		t.Fatalf("неожиданная последовательность попыток: %v", runner.called())
	}

	// Сценарий исчерпания: ход, падающий всегда, — терминальный пропуск.
	turnFail := seedTurn(t, pool, 3003, "private", "ход с постоянным сбоем")
	runner2 := &fakeRunner{pool: pool, fails: map[int64]int{turnFail: 1 << 30}}
	w2 := New(Deps{Pool: pool, Runner: runner2, Journal: journal,
		Logger: log.New(discardWriter{}, "", 0)},
		Options{PollInterval: 10 * time.Millisecond, RetryBase: 10 * time.Millisecond, RetryMax: 20 * time.Millisecond, MaxAttempts: 2})
	go func() { _ = w2.Run(runCtx) }()

	eventually(t, "исчерпание попыток зафиксировано пропуском", func() bool {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM turns WHERE id = $1`, turnFail).Scan(&status); err != nil {
			return false
		}
		return status == "skipped"
	})
	w2.StopDrain()
	<-w2.Done()

	// turn_skipped в журнале (инвариант 5: пропуск фиксируется явно).
	evs, err := journal.History(ctx, turnFail)
	if err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	skipped := false
	for _, ev := range evs {
		if ev.EventType() == "turn_skipped" {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("turn_skipped нет в журнале хода %d", turnFail)
	}
}

// budgetRunner — двойной исполнитель, отвечающий исчерпанием дневного бюджета
// (§16), пока тест не разрешит успех: имитирует вопрос в конце лимитного дня.
type budgetRunner struct {
	pool      *runtime.Pool
	mu        sync.Mutex
	exhausted bool
	calls     int
}

func (r *budgetRunner) Run(ctx context.Context, in character.Input) (character.Outcome, error) {
	r.mu.Lock()
	r.calls++
	exhausted := r.exhausted
	r.mu.Unlock()
	if exhausted {
		// Ошибка хода оборачивает ErrBudgetExhausted (%w), как Runner из middleware.
		return character.Outcome{}, fmt.Errorf("ход %d: генерация не удалась: %w", in.TurnID, runtime.ErrBudgetExhausted)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE turns SET status = 'applied' WHERE id = $1`, in.TurnID); err != nil {
		return character.Outcome{}, err
	}
	return character.Outcome{}, nil
}

func (r *budgetRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *budgetRunner) allow() {
	r.mu.Lock()
	r.exhausted = false
	r.mu.Unlock()
}

// deferUntil — локальная пауза повтора хода (ok=false — паузы нет).
func deferUntil(w *Worker, turnID int64) (time.Time, bool) {
	w.notBeforeMu.Lock()
	defer w.notBeforeMu.Unlock()
	nb, ok := w.notBefore[turnID]
	return nb, ok
}

// clearDefer снимает локальную паузу — как рестарт процесса (notBefore живёт
// в памяти) или наступление срока после границы суток.
func clearDefer(w *Worker, turnID int64) {
	w.notBeforeMu.Lock()
	delete(w.notBefore, turnID)
	w.notBeforeMu.Unlock()
}

func assertTurnState(t *testing.T, pool *runtime.Pool, ctx context.Context, turnID int64, wantStatus string, wantAttempts int) {
	t.Helper()
	var status string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status, attempts FROM turns WHERE id = $1`, turnID).
		Scan(&status, &attempts); err != nil {
		t.Fatalf("ход %d не читается: %v", turnID, err)
	}
	if status != wantStatus || attempts != wantAttempts {
		t.Fatalf("ход %d: status=%s attempts=%d, хочу %s/%d", turnID, status, attempts, wantStatus, wantAttempts)
	}
}

func assertNoTurnSkipped(t *testing.T, journal *character.TurnJournal, ctx context.Context, turnID int64) {
	t.Helper()
	evs, err := journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал хода %d не читается: %v", turnID, err)
	}
	for _, ev := range evs {
		if ev.EventType() == "turn_skipped" {
			t.Fatalf("терминальный пропуск не должен появляться: ход %d", turnID)
		}
	}
}

// TestWorkerBudgetExhaustedDefersToNextDay — 16.7: исчерпание дневного бюджета
// на прямом ходе — не сбой и не терминальный пропуск: попытка не сжигается, ход
// стоит в очереди до следующего дня 00:05 UTC (паритет с фоном); после границы
// суток ход доезжает.
func TestWorkerBudgetExhaustedDefersToNextDay(t *testing.T) {
	pool := newWorkerPool(t)
	ctx := context.Background()

	storage := runtime.NewPgStreamStorage(ctx, pool)
	journal := character.NewTurnJournal(storage, nil)

	turnID := seedTurn(t, pool, 3004, "private", "вопрос в конце лимитного дня")

	runner := &budgetRunner{pool: pool, exhausted: true}
	w := New(Deps{Pool: pool, Runner: runner, Journal: journal,
		Logger: log.New(discardWriter{}, "", 0)},
		Options{PollInterval: 10 * time.Millisecond, RetryBase: 20 * time.Millisecond, RetryMax: 100 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	go func() { _ = w.Run(runCtx) }()

	// Первая встреча с бюджетом: откладывание до завтра 00:05 UTC, попытка возвращена.
	eventually(t, "ход отложен по бюджету", func() bool {
		_, ok := deferUntil(w, turnID)
		return ok
	})
	nb, _ := deferUntil(w, turnID)
	if nb.UTC().Hour() != 0 || nb.UTC().Minute() != 5 || !nb.After(started) {
		t.Fatalf("ход должен ждать следующего дня 00:05 UTC, ждёт до %v", nb.UTC())
	}
	assertTurnState(t, pool, ctx, turnID, "created", 0)
	assertNoTurnSkipped(t, journal, ctx, turnID)

	// «Рестарт» теряет notBefore — вторая встреча с бюджетом: попытки всё ещё
	// не сжигаются, бесконечного терминального пропуска нет.
	clearDefer(w, turnID)
	eventually(t, "вторая встреча с бюджетом", func() bool {
		return runner.callCount() >= 2
	})
	eventually(t, "ход снова отложен", func() bool {
		_, ok := deferUntil(w, turnID)
		return ok
	})
	assertTurnState(t, pool, ctx, turnID, "created", 0)
	assertNoTurnSkipped(t, journal, ctx, turnID)

	// Граница суток прошла (пауза снята, как сделал бы рестарт после 00:05) и
	// бюджет нового дня доступен: ход доезжает, попытка ровно одна — успешная.
	runner.allow()
	clearDefer(w, turnID)
	eventually(t, "ход выполнен на новом дне", func() bool {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM turns WHERE id = $1`, turnID).Scan(&status); err != nil {
			return false
		}
		return status == "applied"
	})
	w.StopDrain()
	<-w.Done()

	assertTurnState(t, pool, ctx, turnID, "applied", 1)
	assertNoTurnSkipped(t, journal, ctx, turnID)
	if got := runner.callCount(); got != 3 {
		t.Fatalf("исполнитель вызван %d раз, хочу 3 (две бюджетные встречи + успех)", got)
	}
}

func orderEqual(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("условие не выполнено за 10 с: %s", what)
}
