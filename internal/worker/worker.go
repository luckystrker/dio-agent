// Package worker — единственный когнитивный worker (§5.2): в каждый момент
// выполняется не более одного модельного задания персонажа, поэтому следующий
// ход читает уже применённые результаты предыдущего.
//
// Очередь — таблица turns (статус created, есть события пачки): очередь в
// памяти хранит только пробуждение, backlog живёт в БД (§5.2, инвариант 3).
// Приоритет: прямое обращение (личка) раньше группового; внутри чата — порядок
// по id; между чатами — FIFO (каждый ход обрабатывается один раз, один чат
// не занимает worker бесконечно). Фон (9.6) откладывается сам, пока очередь
// прямых обращений непуста (§5.2), — здесь это не дублируется.
//
// Повтор хода после транзитного сбоя — ограниченный (§12.2): attempts растёт
// при каждом claim, пауза повтора растёт экспоненциально (§12.1 retry_wait);
// исчерпание лимита — терминальный пропуск (инвариант 5). Пауза между
// попытками живёт в памяти процесса: после падения ход подбирается сразу,
// попытки в БД не теряются.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

// Стартовые значения повторов (§12.1, §5.2; паритет TS worker.ts).
const (
	// DefaultPollInterval — пауза опроса очереди, когда работы нет.
	DefaultPollInterval = 500 * time.Millisecond
	// DefaultRetryBase — база экспоненциальной паузы повтора.
	DefaultRetryBase = 2 * time.Second
	// DefaultRetryMax — потолок паузы повтора.
	DefaultRetryMax = 5 * time.Minute
	// DefaultMaxAttempts — предел попыток одного хода (§12.2 «повторить
	// ограниченно»; паритет WORK_MAX_ATTEMPTS TS).
	DefaultMaxAttempts = 3
)

// Options — параметры цикла (нули — значения по умолчанию; тесты сжимают).
type Options struct {
	PollInterval time.Duration
	RetryBase    time.Duration
	RetryMax     time.Duration
	MaxAttempts  int
}

func (o Options) withDefaults() Options {
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.RetryBase <= 0 {
		o.RetryBase = DefaultRetryBase
	}
	if o.RetryMax <= 0 {
		o.RetryMax = DefaultRetryMax
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	return o
}

// TurnRunner — исполнитель одного хода (продакшн: *character.Runner).
type TurnRunner interface {
	Run(ctx context.Context, in character.Input) (character.Outcome, error)
}

// Deps — зависимости worker.
type Deps struct {
	Pool    *pgxpool.Pool
	Runner  TurnRunner
	Journal *character.TurnJournal // терминальный пропуск после исчерпания попыток (§12.1)
	Logger  *log.Logger
}

// claimedTurn — захваченный ход.
type claimedTurn struct {
	ID      int64
	ChatID  int64
	Direct  bool
	Attempt int // attempts после захвата
}

// Worker — когнитивный цикл. Run — единственная горутина обработки;
// Wake/Stop безопасны из других горутин.
type Worker struct {
	pool    *pgxpool.Pool
	runner  TurnRunner
	journal *character.TurnJournal
	logger  *log.Logger
	opts    Options

	// notBefore — локальные паузы повтора после сбоя (turn_id → срок).
	notBeforeMu sync.Mutex
	notBefore   map[int64]time.Time

	// wake — уведомление о новом ходе (буферы чатов); drain — стоп приёма.
	wake  chan struct{}
	drain chan struct{}
	done  chan struct{}
}

// New строит worker.
func New(deps Deps, opts Options) *Worker {
	logger := deps.Logger
	if logger == nil {
		logger = log.New(discardWriter{}, "", 0)
	}
	return &Worker{
		pool:      deps.Pool,
		runner:    deps.Runner,
		journal:   deps.Journal,
		logger:    logger,
		opts:      opts.withDefaults(),
		notBefore: map[int64]time.Time{},
		wake:      make(chan struct{}, 1),
		drain:     make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// discardWriter — io.Writer в никуда (log.New(nil,…) паникует на записи).
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// Wake будит цикл (буфер закрыл порцию — новый ход в очереди).
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// StopDrain останавливает приём новой работы; текущий ход дорабатывает
// (graceful shutdown §5.3: «остановить приём → ограниченно дождаться
// текущих»). Done закрывается, когда цикл вышел.
func (w *Worker) StopDrain() {
	select {
	case <-w.drain:
	default:
		close(w.drain)
	}
}

// Done — канал завершения цикла Run.
func (w *Worker) Done() <-chan struct{} { return w.done }

// Run крутит цикл до отмены контекста или drain. Ошибка очереди (БД) не
// роняет процесс: лог и пауза с удвоенным интервалом.
func (w *Worker) Run(ctx context.Context) error {
	defer close(w.done)
	w.logger.Printf("когнитивный worker запущен (poll %v, retry %v..%v, максимум попыток %d)",
		w.opts.PollInterval, w.opts.RetryBase, w.opts.RetryMax, w.opts.MaxAttempts)

	for {
		if ctx.Err() != nil || w.drained() {
			w.logger.Printf("когнитивный worker остановлен")
			return nil
		}

		turn, err := w.claimNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logger.Printf("worker: очередь не читается, повтор после %v: %v", 2*w.opts.PollInterval, err)
			if !w.sleep(ctx, 2*w.opts.PollInterval) {
				return nil
			}
			continue
		}
		if turn == nil {
			if !w.waitIdle(ctx) {
				return nil
			}
			continue
		}

		w.process(ctx, turn)
		// Сразу пробуем следующий ход: спать нужно только на пустой очереди.
	}
}

func (w *Worker) drained() bool {
	select {
	case <-w.drain:
		return true
	default:
		return false
	}
}

// waitIdle ждёт работу: wake (новый ход), паузу опроса, отмену или drain.
func (w *Worker) waitIdle(ctx context.Context) bool {
	timer := time.NewTimer(w.opts.PollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-w.drain:
		return false
	case <-w.wake:
		return true
	case <-timer.C:
		return true
	}
}

// sleep ждёт паузу; false — контекст отменён.
func (w *Worker) sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// claimNext берёт следующий ход очереди: прямое обращение раньше группового
// (приоритеты §5.2), внутри — порядок по id. Захват — CAS attempts (повторный
// прогон того же хода после сбоя видит выросший номер попытки); ходы на
// локальной паузе повтора пропускаются до срока.
func (w *Worker) claimNext(ctx context.Context) (*claimedTurn, error) {
	rows, err := w.pool.Query(ctx, `
		SELECT t.id, t.chat_id, (e.chat_type = 'private') AS direct
		FROM turns t
		JOIN LATERAL (
		  SELECT chat_type FROM social_events WHERE turn_id = t.id ORDER BY id LIMIT 1
		) e ON true
		WHERE t.status = 'created'
		ORDER BY direct DESC, t.id
		LIMIT 20`)
	if err != nil {
		return nil, fmt.Errorf("worker: очередь не читается: %w", err)
	}
	defer rows.Close()

	w.notBeforeMu.Lock()
	defer w.notBeforeMu.Unlock()
	now := time.Now()
	for rows.Next() {
		var t claimedTurn
		if err := rows.Scan(&t.ID, &t.ChatID, &t.Direct); err != nil {
			return nil, fmt.Errorf("worker: строка очереди не читается: %w", err)
		}
		if nb, ok := w.notBefore[t.ID]; ok && now.Before(nb) {
			continue // на паузе повтора
		}
		delete(w.notBefore, t.ID)
		// Захват: только если ход всё ещё created (гонку с повторным запуском
		// закрывает CAS; при singleton это страховка).
		var attempt int
		err := w.pool.QueryRow(ctx,
			`UPDATE turns SET attempts = attempts + 1 WHERE id = $1 AND status = 'created' RETURNING attempts`,
			t.ID).Scan(&attempt)
		if err != nil {
			continue // забран другим (нет — в singleton невозможно), берём следующий
		}
		t.Attempt = attempt
		return &t, nil
	}
	return nil, rows.Err()
}

// process обрабатывает один ход и фиксирует служебный статус (журнал хода —
// источник истины, turns.status — видимость для recovery-таблицы §12.2).
func (w *Worker) process(ctx context.Context, turn *claimedTurn) {
	batch, err := w.loadBatch(ctx, turn.ID)
	if err != nil {
		w.fail(ctx, turn, err)
		return
	}
	scope := memory.ChatScope(fmt.Sprintf("%d", turn.ChatID))
	out, err := w.runner.Run(ctx, character.Input{
		TurnID:  turn.ID,
		ChatID:  turn.ChatID,
		ScopeID: scope,
		Batch:   batch,
		Direct:  turn.Direct,
		Attempt: turn.Attempt,
	})

	if err != nil {
		if ctx.Err() != nil {
			return // shutdown: ход остаётся в очереди, recovery подберёт (§12.2)
		}
		// Исчерпание дневного бюджета (§16) — не сбой хода: в полночь пул
		// обнуляется, ход откладывается до завтра, а не теряется (решение 16.7,
		// паритет с фоном). Ошибка хода несёт ErrBudgetExhausted через %w.
		if errors.Is(err, runtime.ErrBudgetExhausted) {
			w.deferBudgetExhausted(ctx, turn)
			return
		}
		w.fail(ctx, turn, err)
		return
	}

	switch {
	case out.Skipped:
		// Терминальный пропуск записал Runner в журнал; служебный статус — здесь.
		if _, uerr := w.pool.Exec(ctx,
			`UPDATE turns SET status = 'skipped' WHERE id = $1 AND status <> 'applied'`, turn.ID); uerr != nil {
			w.logger.Printf("ход %d: статус skipped не записан: %v", turn.ID, uerr)
		}
		w.logger.Printf("ход %d пропущен терминально: %s", turn.ID, out.SkipReason)
	case out.AlreadyApplied:
		w.logger.Printf("ход %d уже применён — повторный прогон ничего не менял", turn.ID)
	default:
		w.logger.Printf("ход %d выполнен: recovered=%v памяти=%d доставки=%d",
			turn.ID, out.Recovered, len(out.Apply.MemoryIDs), len(out.Apply.OutboxIDs))
	}
}

// loadBatch читает события пачки хода (§6.2: принадлежность событий turn —
// в БД, очередь в памяти только имена).
func (w *Worker) loadBatch(ctx context.Context, turnID int64) ([]character.BatchEvent, error) {
	rows, err := w.pool.Query(ctx, `
		SELECT id, coalesce(actor_id, 0), coalesce(actor_name, ''), coalesce(text, ''), raw_update
		FROM social_events WHERE turn_id = $1 ORDER BY id`, turnID)
	if err != nil {
		return nil, fmt.Errorf("ход %d: события пачки не читаются: %w", turnID, err)
	}
	defer rows.Close()

	var batch []character.BatchEvent
	for rows.Next() {
		var ev character.BatchEvent
		if err := rows.Scan(&ev.EventID, &ev.ActorID, &ev.ActorName, &ev.Text, &ev.Raw); err != nil {
			return nil, fmt.Errorf("ход %d: событие пачки не читается: %w", turnID, err)
		}
		batch = append(batch, ev)
	}
	return batch, rows.Err()
}

// fail — реакция на транзитный сбой хода: пауза повтора растёт с номером
// попытки (§12.1 retry_wait); исчерпание лимита — терминальный пропуск
// (инвариант 5: событие не теряется, пропуск фиксируется явно).
func (w *Worker) fail(ctx context.Context, turn *claimedTurn, runErr error) {
	if turn.Attempt >= w.opts.MaxAttempts {
		// Причина — для журнала и наблюдаемости: текст ошибки провайдера может
		// быть простынёй (429-блобы OpenRouter), в логах достаточно хвоста.
		reason := fmt.Sprintf("превышен лимит попыток (%d): %v", turn.Attempt, truncateRunes(fmt.Sprint(runErr), 300))
		if w.journal != nil {
			if jerr := w.journal.WriteTurnSkipped(ctx, turn.ID, "worker", reason); jerr != nil {
				w.logger.Printf("ход %d: turn_skipped не записан: %v", turn.ID, jerr)
			}
		}
		if _, uerr := w.pool.Exec(ctx,
			`UPDATE turns SET status = 'skipped' WHERE id = $1 AND status <> 'applied'`, turn.ID); uerr != nil {
			w.logger.Printf("ход %d: статус skipped не записан: %v", turn.ID, uerr)
		}
		w.logger.Printf("ход %d: %s", turn.ID, reason)
		return
	}

	pause := w.backoff(turn.Attempt)
	w.notBeforeMu.Lock()
	w.notBefore[turn.ID] = time.Now().Add(pause)
	w.notBeforeMu.Unlock()
	w.logger.Printf("ход %d вернулся в очередь с паузой %v (попытка %d): %v",
		turn.ID, pause, turn.Attempt, runErr)
}

// deferBudgetExhausted — исчерпание дневного бюджета на прямом ходе (§16):
// попытка не сжигается (claim её добавил — возвращаем), ход стоит в очереди
// до следующего дня 00:05 UTC — общей с фоном границы суток (runtime.NextDay,
// решение 16.7). notBefore живёт в памяти процесса: рестарт делает ход
// доступным сразу, но попытки бюджетной ошибкой не тратятся — вторая встреча
// снова отложит, бесконечного терминального пропуска нет.
func (w *Worker) deferBudgetExhausted(ctx context.Context, turn *claimedTurn) {
	if _, err := w.pool.Exec(ctx,
		`UPDATE turns SET attempts = GREATEST(attempts - 1, 0) WHERE id = $1 AND status = 'created'`, turn.ID); err != nil {
		w.logger.Printf("ход %d: попытка не возвращена после откладывания по бюджету: %v", turn.ID, err)
	}
	notBefore := runtime.NextDay(time.Now())
	w.notBeforeMu.Lock()
	w.notBefore[turn.ID] = notBefore
	w.notBeforeMu.Unlock()
	w.logger.Printf("ход %d отложено до завтра: бюджет (до %s)", turn.ID, notBefore.UTC().Format("2006-01-02 15:04"))
}

// backoff — экспоненциальная пауза: base * 2^(попытка-1), с потолком RetryMax.
func (w *Worker) backoff(attempt int) time.Duration {
	pause := w.opts.RetryBase
	for i := 1; i < attempt && pause < w.opts.RetryMax; i++ {
		pause *= 2
	}
	if pause > w.opts.RetryMax {
		pause = w.opts.RetryMax
	}
	return pause
}

// truncateRunes обрезает строку до n символов (без разрыва рун).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
