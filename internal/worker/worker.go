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
// исчерпание лимита — терминальный провал: status=failed с причиной и событием
// turn_failed (29.1). Пауза между попытками живёт в памяти процесса: после
// падения ход подбирается сразу, попытки в БД не теряются.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

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
	// DefaultTransientMaxAttempts — предел попыток для транзитного отказа
	// провайдера (5xx/429/сеть): 5-минутная недоступность API не должна
	// превращать сообщение в терминальный skipped за 6 секунд (§12.2, M10).
	DefaultTransientMaxAttempts = 8
	// MaxDirectTurnBudgetWait — разумный предел ожидания дневного бюджета для
	// прямого хода (29.2): дольше вопроса в очереди «не бывает» — ход честно
	// пропускается с логом, а не откладывается снова и снова.
	MaxDirectTurnBudgetWait = 12 * time.Hour
	// transientMinPause — минимальная пауза между попытками транзитного
	// отказа провайдера: короткая экспонента 2–4 с выжигает лимит попыток
	// раньше, чем провайдер успевает восстановиться.
	transientMinPause = 30 * time.Second
)

// Options — параметры цикла (нули — значения по умолчанию; тесты сжимают).
type Options struct {
	PollInterval time.Duration
	RetryBase    time.Duration
	RetryMax     time.Duration
	MaxAttempts  int
	// TransientMaxAttempts — предел попыток для транзитных отказов провайдера;
	// 0 — DefaultTransientMaxAttempts.
	TransientMaxAttempts int
	// RecoveryInterval — период возврата зависших pending-событий в ходы;
	// 0 — 30 секунд (M5).
	RecoveryInterval time.Duration
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
	if o.TransientMaxAttempts <= 0 {
		o.TransientMaxAttempts = DefaultTransientMaxAttempts
	}
	if o.RecoveryInterval <= 0 {
		o.RecoveryInterval = 30 * time.Second
	}
	return o
}

// TurnRunner — исполнитель одного хода (продакшн: *character.Runner).
type TurnRunner interface {
	Run(ctx context.Context, in character.Input) (character.Outcome, error)
}

// Deps — зависимости worker.
type Deps struct {
	Pool    *runtime.Pool
	Runner  TurnRunner
	Journal *character.TurnJournal // терминальный пропуск после исчерпания попыток (§12.1)
	Logger  *log.Logger
	// Cognition — общий с фоном семафор модельных заданий (§5.2); nil — без него.
	Cognition *runtime.CognitionLock
	// PendingRecovery — периодический возврат pending-событий в ходы (M5):
	// транзиентный сбой закрытия буфера не должен ждать рестарта процесса.
	PendingRecovery func(ctx context.Context) error
}

// claimedTurn — захваченный ход.
type claimedTurn struct {
	ID        int64
	ChatID    int64
	Direct    bool
	CreatedAt time.Time
	Attempt   int // attempts после захвата
}

// Worker — когнитивный цикл. Run — единственная горутина обработки;
// Wake/Stop безопасны из других горутин.
type Worker struct {
	pool    *runtime.Pool
	runner  TurnRunner
	journal *character.TurnJournal
	logger  *log.Logger
	opts    Options
	// cognition — общий с фоном семафор модельных заданий (§5.2).
	cognition       *runtime.CognitionLock
	pendingRecovery func(ctx context.Context) error
	// lastPendingRecovery — когда последний раз шёл recovery pending (только
	// горутина Run).
	lastPendingRecovery time.Time

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
		pool:            deps.Pool,
		runner:          deps.Runner,
		journal:         deps.Journal,
		logger:          logger,
		opts:            opts.withDefaults(),
		cognition:       deps.Cognition,
		pendingRecovery: deps.PendingRecovery,
		notBefore:       map[int64]time.Time{},
		wake:            make(chan struct{}, 1),
		drain:           make(chan struct{}),
		done:            make(chan struct{}),
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
		w.recoverPendingDue(ctx)

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

// recoverPendingDue — периодический recovery pending-событий (M5): закрытие
// буфера, упавшее на транзиентном сбое, больше не ждёт рестарта. Свежие
// события не трогаются — их отбирает age-фильтр в callback.
func (w *Worker) recoverPendingDue(ctx context.Context) {
	if w.pendingRecovery == nil {
		return
	}
	if time.Since(w.lastPendingRecovery) < w.opts.RecoveryInterval {
		return
	}
	w.lastPendingRecovery = time.Now()
	if err := w.pendingRecovery(ctx); err != nil {
		w.logger.Printf("worker: recovery pending-событий не удался: %v", err)
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
// локальной паузе повтора и на сроке not_before (отложенные по бюджету, 29.2)
// пропускаются до срока. Ход чата не берётся, пока
// более ранний ход того же чата ещё created (M2): иначе после сбоя ответ на
// позднее сообщение уходил раньше ответа на раннее, а события раннего хода
// попадали в историю позднего как обработанные.
//
// Строки очереди дочитываются и закрываются ДО CAS (M9): CAS на открытом
// курсоре занимает второе соединение, и при DB_MAX_CONNS=1 или насыщенном
// пуле захват молча не проходил.
func (w *Worker) claimNext(ctx context.Context) (*claimedTurn, error) {
	rows, err := w.pool.Query(ctx, `
		SELECT t.id, t.chat_id, (e.chat_type = 'private') AS direct, t.created_at
		FROM turns t
		JOIN LATERAL (
		  SELECT chat_type FROM social_events WHERE turn_id = t.id ORDER BY id LIMIT 1
		) e ON true
		WHERE t.status = 'created'
		  AND (t.not_before IS NULL OR t.not_before <= now())
		  AND NOT EXISTS (
		    SELECT 1 FROM turns prev
		    WHERE prev.chat_id = t.chat_id
		      AND prev.status = 'created'
		      AND prev.id < t.id
		      AND EXISTS (SELECT 1 FROM social_events pe WHERE pe.turn_id = prev.id)
		  )
		ORDER BY direct DESC, t.id
		LIMIT 20`)
	if err != nil {
		return nil, fmt.Errorf("worker: очередь не читается: %w", err)
	}
	var candidates []claimedTurn
	for rows.Next() {
		var t claimedTurn
		if err := rows.Scan(&t.ID, &t.ChatID, &t.Direct, &t.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("worker: строка очереди не читается: %w", err)
		}
		candidates = append(candidates, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worker: очередь не дочитана: %w", err)
	}

	w.notBeforeMu.Lock()
	defer w.notBeforeMu.Unlock()
	now := time.Now()
	for _, t := range candidates {
		if nb, ok := w.notBefore[t.ID]; ok && now.Before(nb) {
			continue // на паузе повтора
		}
		// Захват: только если ход всё ещё created (гонку с повторным запуском
		// закрывает CAS; при singleton это страховка).
		var attempt int
		err := w.pool.QueryRow(ctx,
			`UPDATE turns SET attempts = attempts + 1, not_before = NULL
			 WHERE id = $1 AND status = 'created' RETURNING attempts`,
			t.ID).Scan(&attempt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // забран другим (в singleton невозможно), берём следующий
		}
		if err != nil {
			// Ошибка CAS (таймаут acquire, обрыв соединения) — не «забрал
			// другой»: молчаливый пропуск при насыщенном пуле оставлял бота
			// без ходов и без единой строки в логе (M9).
			return nil, fmt.Errorf("worker: ход %d не захвачен: %w", t.ID, err)
		}
		delete(w.notBefore, t.ID)
		t.Attempt = attempt
		return &t, nil
	}
	return nil, nil
}

// process обрабатывает один ход и фиксирует служебный статус (журнал хода —
// источник истины, turns.status — видимость для recovery-таблицы §12.2).
func (w *Worker) process(ctx context.Context, turn *claimedTurn) {
	batch, err := w.loadBatch(ctx, turn.ID)
	if err != nil {
		if ctx.Err() != nil {
			// Отмена во время чтения пачки — не сбой хода: попытка не сгорает.
			w.releaseAttempt(turn)
			return
		}
		w.fail(ctx, turn, err)
		return
	}
	scope := memory.ChatScope(fmt.Sprintf("%d", turn.ChatID))
	release, ok := w.cognition.Acquire(ctx)
	if !ok {
		// Отмена, пока ждали семафор: ход не выполнялся — попытка не сгорает.
		w.releaseAttempt(turn)
		return
	}
	defer release()
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
			// shutdown: ход остаётся в очереди, recovery подберёт (§12.2),
			// а попытка, сожжённая захватом, возвращается (minor 9): несколько
			// рестартов подряд не должны перевести ход в skipped без ошибки.
			w.releaseAttempt(turn)
			return
		}
		// Исчерпание дневного бюджета (§16) — не сбой хода: в полночь пул
		// обнуляется, ход откладывается до завтра, а не теряется (решение 16.7,
		// паритет с фоном). Ошибка хода несёт ErrBudgetExhausted через %w.
		// Срок откладывания — в БД (not_before, 29.2): рестарт не делает ход
		// доступным раньше сброса счётчика. Прямой ход, провисевший дольше
		// разумного предела, пропускается честно — ждать вечно нечего.
		if errors.Is(err, runtime.ErrBudgetExhausted) {
			if turn.Direct && time.Since(turn.CreatedAt) >= MaxDirectTurnBudgetWait {
				w.skipStaleBudgetTurn(ctx, turn)
				return
			}
			w.deferTurn(ctx, turn, runtime.NextDay(time.Now()), "бюджет")
			return
		}
		// Модель не настроена: конфигурация приходит рестартом процесса.
		// Терминальный skipped за 3 попытки терял бы сообщение пользователя
		// (M10); откладываем, не расходуя попытку.
		if errors.Is(err, character.ErrModelNotConfigured) {
			w.deferTurn(ctx, turn, time.Now().Add(w.opts.RetryMax), "модель не настроена")
			return
		}
		w.fail(ctx, turn, err)
		return
	}

	switch {
	case out.Failed:
		// Терминальный провал (29.1) уже записан в журнал; служебный статус —
		// failed с причиной: воркер такие не берёт, recovery не воскрешает.
		w.markFailed(ctx, turn.ID, out.FailReason)
		w.logger.Printf("ход %d провален терминально: %s", turn.ID, out.FailReason)
	case out.Skipped:
		// Терминальный пропуск записал Runner в журнал; служебный статус — здесь.
		if _, uerr := w.pool.Exec(ctx,
			`UPDATE turns SET status = 'skipped' WHERE id = $1 AND status <> 'applied'`, turn.ID); uerr != nil {
			w.logger.Printf("ход %d: статус skipped не записан: %v", turn.ID, uerr)
		}
		w.logger.Printf("ход %d пропущен терминально: %s", turn.ID, out.SkipReason)
	case out.AlreadyApplied:
		w.logger.Printf("ход %d уже применён — повторный прогон ничего не менял", turn.ID)
		// Успешный ход здесь не логируется (26.1): полную строку «ход N
		// выполнен: decision=… recovered=…» пишет character.Runner в turn.go.
		// Своей информации у worker нет — короткая копия была вторым шумом
		// на каждый ход. Если когда-нибудь появится путь применения без
		// Runner, его итог должен лечь в эту же строку, а не рядом.
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
// попытки (§12.1 retry_wait); исчерпание лимита — терминальный провал:
// статус failed с причиной и событие turn_failed в журнале (29.1). Пропуск
// (skipped) остаётся для сознательных решений хода, провал — для ошибок
// исполнения, которые повтором не лечатся.
func (w *Worker) fail(ctx context.Context, turn *claimedTurn, runErr error) {
	if turn.Attempt >= w.attemptLimitFor(runErr) {
		// Причина — для журнала и наблюдаемости: текст ошибки провайдера может
		// быть простынёй (429-блобы OpenRouter), в логах достаточно хвоста.
		reason := fmt.Sprintf("превышен лимит попыток (%d): %v", turn.Attempt, truncateRunes(fmt.Sprint(runErr), 300))
		if w.journal != nil {
			if jerr := w.journal.WriteTurnFailed(ctx, turn.ID, "worker", reason); jerr != nil {
				w.logger.Printf("ход %d: turn_failed не записан: %v", turn.ID, jerr)
			}
		}
		w.markFailed(ctx, turn.ID, reason)
		w.logger.Printf("ход %d: %s", turn.ID, reason)
		return
	}

	pause := w.pauseFor(turn.Attempt, runErr)
	w.notBeforeMu.Lock()
	w.notBefore[turn.ID] = time.Now().Add(pause)
	w.notBeforeMu.Unlock()
	w.logger.Printf("ход %d вернулся в очередь с паузой %v (попытка %d): %v",
		turn.ID, pause, turn.Attempt, runErr)
}

// markFailed переводит строку хода в терминальный статус failed с причиной.
// Применённый ход не перезаписывается (CAS по статусу): провал после
// успешного применения невозможен.
func (w *Worker) markFailed(ctx context.Context, turnID int64, reason string) {
	if _, err := w.pool.Exec(ctx,
		`UPDATE turns SET status = 'failed', failure_reason = $2
		 WHERE id = $1 AND status <> 'applied'`, turnID, reason); err != nil {
		w.logger.Printf("ход %d: статус failed не записан: %v", turnID, err)
	}
}

// attemptLimitFor — предел попыток: для транзитного отказа провайдера он выше,
// чтобы 5-минутная недоступность не превращалась в терминальный пропуск (M10).
func (w *Worker) attemptLimitFor(runErr error) int {
	if transientModelError(runErr) {
		return w.opts.TransientMaxAttempts
	}
	return w.opts.MaxAttempts
}

// pauseFor — пауза повтора: у транзитного отказа провайдера не короче
// transientMinPause (M10).
func (w *Worker) pauseFor(attempt int, runErr error) time.Duration {
	pause := w.backoff(attempt)
	if transientModelError(runErr) && pause < transientMinPause {
		pause = transientMinPause
	}
	return pause
}

// statusCodeRe достаёт HTTP-код из текста ошибки OpenAI-совместимого слоя
// ago («config: chat completions returned 502: …»): типизированных ошибок
// провайдер не отдаёт.
var statusCodeRe = regexp.MustCompile(`returned (\d{3})`)

// transientModelError — отказ провайдера, который имеет смысл повторить:
// 5xx, 429 и сетевые ошибки. Постоянные 4xx и отмена хода — нет.
func transientModelError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := err.Error()
	if m := statusCodeRe.FindStringSubmatch(msg); len(m) == 2 {
		code, perr := strconv.Atoi(m[1])
		if perr == nil {
			return code == 429 || code >= 500
		}
	}
	// Сетевые формулировки транспорта без типизированной ошибки.
	for _, marker := range []string{
		"connection reset", "connection refused", "no such host",
		"i/o timeout", "TLS handshake timeout", "unexpected EOF",
		"network is unreachable", "broken pipe",
	} {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// releaseAttempt возвращает попытку, сожжённую захватом, если ход не
// обрабатывался (отмена при shutdown). Запись — на отдельном контексте:
// runCtx уже отменён, а пул ещё жив до конца Stop.
func (w *Worker) releaseAttempt(turn *claimedTurn) {
	qctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := w.pool.Exec(qctx,
		`UPDATE turns SET attempts = GREATEST(attempts - 1, 0) WHERE id = $1 AND status = 'created'`, turn.ID); err != nil {
		w.logger.Printf("ход %d: попытка не возвращена после остановки: %v", turn.ID, err)
	}
}

// deferTurn — отложить ход без расхода попытки: бюджет откладывается до
// следующей границы суток (решение 16.7), ненастроенная модель — на RetryMax
// (конфигурация приходит рестартом, попытка не должна сгорать). Срок пишется
// в строку хода (not_before, 29.2) — рестарт процесса его не теряет; та же
// пауза дублируется в памяти, чтобы цикл не перечитывал ход каждый опрос.
func (w *Worker) deferTurn(ctx context.Context, turn *claimedTurn, notBefore time.Time, reason string) {
	if _, err := w.pool.Exec(ctx,
		`UPDATE turns SET attempts = GREATEST(attempts - 1, 0), not_before = $2
		 WHERE id = $1 AND status = 'created'`, turn.ID, notBefore); err != nil {
		w.logger.Printf("ход %d: срок откладывания (%s) не записан: %v", turn.ID, reason, err)
	}
	w.notBeforeMu.Lock()
	w.notBefore[turn.ID] = notBefore
	w.notBeforeMu.Unlock()
	w.logger.Printf("ход %d отложен (%s) до %s", turn.ID, reason, notBefore.UTC().Format("2006-01-02 15:04"))
}

// skipStaleBudgetTurn — прямой ход, провисевший в ожидании бюджета дольше
// MaxDirectTurnBudgetWait (29.2): честный терминальный пропуск с причиной и
// логом вместо бесконечного переноса на завтра.
func (w *Worker) skipStaleBudgetTurn(ctx context.Context, turn *claimedTurn) {
	reason := fmt.Sprintf("дневной бюджет моделей: прямой ход ждёт с %s дольше предела %d ч — терминальный пропуск (29.2)",
		turn.CreatedAt.UTC().Format("2006-01-02 15:04"), int(MaxDirectTurnBudgetWait.Hours()))
	if w.journal != nil {
		if jerr := w.journal.WriteTurnSkipped(ctx, turn.ID, "budget", reason); jerr != nil {
			w.logger.Printf("ход %d: turn_skipped не записан: %v", turn.ID, jerr)
		}
	}
	if _, uerr := w.pool.Exec(ctx,
		`UPDATE turns SET status = 'skipped' WHERE id = $1 AND status <> 'applied'`, turn.ID); uerr != nil {
		w.logger.Printf("ход %d: статус skipped не записан: %v", turn.ID, uerr)
	}
	w.logger.Printf("ход %d пропущен терминально: %s", turn.ID, reason)
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
