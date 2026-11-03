// Delivery loop (§13): работает независимо от когнитивного worker, сохраняет
// порядок внутри чата и не блокирует cognition. Политика неопределённости
// (§13.2): подтверждённый временный отказ — retry_wait (уважая retry_after);
// подтверждённая постоянная ошибка — failed; timeout/обрыв после возможной
// отправки — unknown без автоповтора, зависимые пузыри отменяются. Перед
// каждой отправкой — проверка устаревания: новые события чата отменяют
// оставшиеся bubbles (§13.3). Ошибка БД не зависляет пузырь в sending
// (16.3): до попытки отправки — возврат в retry_wait, после отправки без
// записи исхода — unknown; зависшее sending подбирает периодический recovery
// (§12.2). Поведение — src/telegram/delivery.ts.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// DB — минимальный интерфейс запросов к БД для проверки устаревания (§13.3);
// реализуется *pgxpool.Pool.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// staleSendingAfter — sending старше этого возраста считается зависшим:
// recovery §12.2 переводит его в unknown (не отправлять вслепую). Тот же
// порог, что у startup-recovery приложения (internal/app).
const staleSendingAfter = time.Minute

// maxRetryAfterWait — потолок паузы 429 (17.6): Telegram может прислать
// retry_after в час — держать строку в очереди час по одной ошибке флуда
// затягивает доставку остального чата. Пауза выше потолка обрезается до него.
const maxRetryAfterWait = 5 * time.Minute

// persistTimeout — потолок записи исхода захваченной строки. Запись исхода
// (sent/retry_wait/failed/unknown/возврат в очередь) не должна отменяться
// вместе с runCtx: иначе при shutdown строка остаётся в sending и следующий
// старт переводит её в unknown, теряя заведомо не отправленный ответ (C1).
const persistTimeout = 5 * time.Second

// persistCtx — контекст записи исхода: не наследует отмену цикла доставки,
// но ограничен по времени.
func (l *DeliveryLoop) persistCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), persistTimeout)
}

// DeliveryOptions — настройки delivery loop.
type DeliveryOptions struct {
	// Interval — период такта (по умолчанию 500 мс, паритет TS).
	Interval time.Duration
	// BatchSize — сколько строк захватывается за такт (10, паритет TS).
	BatchSize int
	// MaxAttempts — предел попыток для временных отказов (5, паритет TS).
	MaxAttempts int
	// RetryBase — база экспоненциальной паузы retry (2 с, паритет TS).
	RetryBase time.Duration
	// BubbleGap — минимальный интервал между bubbles одного turn
	// (§13.3: 300–1500 мс; по умолчанию 800 мс — середина диапазона TS).
	BubbleGap time.Duration
	// RecoveryInterval — период recovery зависшего sending из такта (§12.2,
	// 16.3; по умолчанию 30 с): пузырь, зависший в живом процессе, подбирается
	// без рестарта. Порог staleness — staleSendingAfter, настройкой не меняется.
	RecoveryInterval time.Duration
}

func (o DeliveryOptions) withDefaults() DeliveryOptions {
	if o.Interval <= 0 {
		o.Interval = 500 * time.Millisecond
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 10
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 5
	}
	if o.RetryBase <= 0 {
		o.RetryBase = 2 * time.Second
	}
	if o.BubbleGap <= 0 {
		o.BubbleGap = 800 * time.Millisecond
	}
	if o.RecoveryInterval <= 0 {
		o.RecoveryInterval = 30 * time.Second
	}
	return o
}

// DeliveryLoop — отдельный цикл доставки из outbox.
type DeliveryLoop struct {
	db     DB
	repo   *OutboxRepo
	api    API
	logger *log.Logger
	opts   DeliveryOptions

	// lastRecovery — когда последний раз шёл recovery зависшего sending
	// (§12.2, 16.3); читается/пишется только из горутины такта.
	lastRecovery time.Time

	// lastSent[turnID] — когда отправлен последний bubble turn (для паузы
	// §13.3 между bubbles, в том числе между тактами).
	mu       sync.Mutex
	lastSent map[int64]time.Time
}

// NewDeliveryLoop строит цикл доставки. db — пул PostgreSQL (запрос
// устаревания §13.3).
func NewDeliveryLoop(db DB, repo *OutboxRepo, api API, logger *log.Logger, opts DeliveryOptions) *DeliveryLoop {
	if logger == nil {
		logger = log.New(discardWriter{}, "", 0)
	}
	return &DeliveryLoop{
		db:           db,
		repo:         repo,
		api:          api,
		logger:       logger,
		opts:         opts.withDefaults(),
		lastRecovery: time.Now(),
		lastSent:     map[int64]time.Time{},
	}
}

// Run крутит такты до отмены контекста (§5.3: delivery запускается на старте,
// останавливается при shutdown). Такт под recover (16.2): паника становится
// ошибкой такта в логе и не роняет цикл и процесс.
func (l *DeliveryLoop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.tickGuarded(ctx); err != nil {
				l.logger.Printf("delivery: ошибка такта: %v", err)
			}
		}
	}
}

// tickGuarded — один такт доставки под recover: паника возвращается ошибкой
// (со стеком), цикл продолжает следующим тиком (16.2).
func (l *DeliveryLoop) tickGuarded(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("паника такта: %v\n%s", r, debug.Stack())
		}
	}()
	return l.Tick(ctx)
}

// Tick — один такт доставки (отдельным методом для тестируемости):
// периодический recovery зависшего sending (§12.2, 16.3), затем захват пачки
// и отправка по порядку.
func (l *DeliveryLoop) Tick(ctx context.Context) error {
	if err := l.recoverStaleDue(ctx); err != nil {
		return err
	}
	rows, err := l.repo.ClaimDue(ctx, l.opts.BatchSize)
	if err != nil {
		return err
	}
	for i, row := range rows {
		if ctx.Err() != nil {
			// Остановка: к оставшимся строкам отправку не начинали —
			// возвращаем их в очередь, не оставляя захваченными в sending (C1).
			l.requeueAbandoned(rows[i:])
			return ctx.Err()
		}
		l.deliver(ctx, row)
	}
	l.pruneLastSent()
	return nil
}

// recoverStaleDue — периодический recovery зависшего sending (§12.2) из такта
// доставки (16.3): раньше recovery шёл один раз на старте — пузырь, зависший
// в живом процессе (ошибка записи исхода), ждал рестарта. Пока интервал не
// истёк — пропуск (обычный такт не бегает recovery-запросом в БД); истёк —
// sending старше staleSendingAfter переводится в unknown, зависимые
// отменяются. Ошибка — ошибка такта (логируется Run'ом), следующий такт
// попробует снова.
func (l *DeliveryLoop) recoverStaleDue(ctx context.Context) error {
	if time.Since(l.lastRecovery) < l.opts.RecoveryInterval {
		return nil
	}
	touched, err := l.repo.RecoverStaleSending(ctx, staleSendingAfter)
	l.lastRecovery = time.Now()
	if err != nil {
		return fmt.Errorf("recovery sending: %w", err)
	}
	if touched > 0 {
		l.logger.Printf("delivery: recovery перевёл %d зависших sending в unknown/cancelled (§12.2)", touched)
	}
	return nil
}

// deliver отправляет одну единицу доставки с проверками §13.2/§13.3.
func (l *DeliveryLoop) deliver(ctx context.Context, row OutboxItem) {
	// Захваченная пачка могла устареть в памяти: предыдущий пузырь стал
	// unknown и отменил эту. Перепроверяем статус перед отправкой (§13.2).
	status, err := l.repo.StatusOf(ctx, row.ID)
	if err != nil {
		l.requeueOnDBError(ctx, row, fmt.Sprintf("статус строки %d не читается: %v", row.ID, err))
		return
	}
	if status != OutboxSending {
		l.logger.Printf("delivery: строка %d (turn %d, поз. %d) больше не в sending (%s) — пропуск",
			row.ID, row.TurnID, row.Position, status)
		return
	}

	// §13.3: перед отправкой — устаревание. Новые события чата делают
	// продолжение неуместным: хвостовые bubbles отменяются. Первый пузырь —
	// прямой ответ, ради которого ход состоялся: уходит всегда, даже если
	// следом пришло новое — ответить позже лучше, чем не ответить вовсе
	// (портировано из TS-линии c0a986d, 2026-10-31).
	stale, err := l.chatHasNewPendingEvents(ctx, row.ChatID)
	if err != nil {
		l.requeueOnDBError(ctx, row, err.Error())
		return
	}
	if stale && row.Position > 0 {
		cancelled, cerr := l.repo.CancelTurnFrom(ctx, row.TurnID, row.Position, "чат ушёл вперёд — пузыри устарели")
		if cerr != nil {
			// Отмена не записалась — строка возвращается в очередь: следующий
			// такт перепроверит устаревание и отменит как надо (16.3).
			l.requeueOnDBError(ctx, row, fmt.Sprintf("отмена устаревших пузырей turn %d не удалась: %v", row.TurnID, cerr))
			return
		}
		l.logger.Printf("delivery: доставка отменена — чат %d ушёл вперёд (turn %d, отменено %d)",
			row.ChatID, row.TurnID, cancelled)
		return
	}

	// §13.3: пауза между bubbles одного turn (не для первого сообщения).
	l.waitForBubbleGap(ctx, row)
	if ctx.Err() != nil {
		// Пауза прервана остановкой: к отправке не приступали — строка
		// возвращается в очередь, а не остаётся в sending (C1).
		l.requeueAbandoned([]OutboxItem{row})
		return
	}

	switch row.Kind {
	case "reaction":
		if row.TargetMessageID == 0 || row.Reaction == "" {
			qctx, cancel := l.persistCtx()
			defer cancel()
			if ferr := l.repo.MarkFailed(qctx, row.ID, "реакция без emoji/target_message_id"); ferr != nil {
				l.requeueOnDBError(ctx, row, ferr.Error())
			}
			return
		}
		if err := l.api.SetMessageReaction(ctx, row.ChatID, row.TargetMessageID, row.Reaction); err != nil {
			l.handleSendError(ctx, row, err)
			return
		}
		// У реакции нет telegram_message_id — NULL, а не 0 (§13.1).
		qctx, cancel := l.persistCtx()
		err := l.repo.MarkSent(qctx, row.ID, nil)
		cancel()
		if err != nil {
			l.markUnknownAfterDBError(ctx, row, err.Error())
			return
		}
		l.noteSent(row)
		l.logger.Printf("delivery: реакция %s поставлена (turn %d, сообщение %d)",
			row.Reaction, row.TurnID, row.TargetMessageID)
	default:
		messageID, err := l.api.SendMessage(ctx, row.ChatID, row.Text)
		if err != nil {
			l.handleSendError(ctx, row, err)
			return
		}
		qctx, cancel := l.persistCtx()
		err = l.repo.MarkSent(qctx, row.ID, &messageID)
		cancel()
		if err != nil {
			l.markUnknownAfterDBError(ctx, row, err.Error())
			return
		}
		l.noteSent(row)
		l.logger.Printf("delivery: отправлено (turn %d, поз. %d, message_id %d)",
			row.TurnID, row.Position, messageID)
	}
}

// requeueOnDBError — ошибка БД ДО попытки отправки (16.3): захват уже сделан,
// отправки не было — строка возвращается из sending в retry_wait с паузой по
// образцу classifySendError (экспонента по попыткам, предел попыток — failed).
// Без этого пузырь зависал в sending до рестарта: ClaimDue берёт только
// pending/retry_wait. Если и возврат не записался — строка остаётся в sending,
// её подберёт периодический recovery (§12.2).
func (l *DeliveryLoop) requeueOnDBError(ctx context.Context, row OutboxItem, reason string) {
	message := "ошибка БД до отправки: " + reason
	wait, giveUp := dbRetryBackoff(row.Attempts, l.opts.MaxAttempts, l.opts.RetryBase)
	qctx, cancel := l.persistCtx()
	defer cancel()
	if giveUp {
		if err := l.repo.MarkFailed(qctx, row.ID, message); err != nil {
			l.logger.Printf("delivery: failed не записан (строка %d остаётся в sending до recovery): %v", row.ID, err)
			return
		}
		l.logger.Printf("delivery: ошибка БД до отправки, предел попыток — failed (строка %d, turn %d, поз. %d): %s",
			row.ID, row.TurnID, row.Position, reason)
		return
	}
	if err := l.repo.RequeueFromSending(qctx, row.ID, time.Now().Add(wait), message); err != nil {
		l.logger.Printf("delivery: возврат в retry_wait не удался (строка %d остаётся в sending до recovery): %v", row.ID, err)
		return
	}
	l.logger.Printf("delivery: ошибка БД до отправки — возврат в retry_wait, повтор через %v (строка %d, turn %d, поз. %d): %s",
		wait, row.ID, row.TurnID, row.Position, reason)
}

// requeueAbandoned возвращает в очередь строки, к отправке которых не
// приступали из-за остановки процесса (C1): без этого они остаются в sending,
// а старт переводит их в unknown и отменяет весь остаток хода.
func (l *DeliveryLoop) requeueAbandoned(rows []OutboxItem) {
	if len(rows) == 0 {
		return
	}
	qctx, cancel := l.persistCtx()
	defer cancel()
	for _, row := range rows {
		if err := l.repo.RequeueFromSending(qctx, row.ID, time.Now(),
			"остановка процесса до отправки: строка возвращена в очередь"); err != nil {
			l.logger.Printf("delivery: остановка — строка %d не возвращена в очередь (остаётся в sending до recovery): %v",
				row.ID, err)
			continue
		}
		l.logger.Printf("delivery: остановка — строка %d (turn %d, поз. %d) возвращена в очередь",
			row.ID, row.TurnID, row.Position)
	}
}

// markUnknownAfterDBError — ошибка записи ПОСЛЕ подтверждённой отправки
// (MarkSent не записан, 16.3): исход для строки неясен — unknown, не
// отправлять вслепую (§13.2); зависимые пузыри отменяются. Если и эта запись
// не прошла — строка остаётся в sending до периодического recovery (§12.2).
func (l *DeliveryLoop) markUnknownAfterDBError(ctx context.Context, row OutboxItem, reason string) {
	message := "отправка подтверждена, sent не записан: " + reason
	qctx, cancel := l.persistCtx()
	defer cancel()
	changed, cancelled, err := l.repo.MarkUnknown(qctx, row.ID, message)
	if err != nil {
		l.logger.Printf("delivery: unknown не записан (строка %d остаётся в sending до recovery): %v", row.ID, err)
		return
	}
	if changed {
		l.logger.Printf("delivery: sent не записан после отправки — исход неопределён, зависимых отменено %d (строка %d, turn %d, поз. %d)",
			cancelled, row.ID, row.TurnID, row.Position)
	}
}

// dbRetryBackoff — пауза повтора после ошибки БД до отправки (16.3), по
// образцу classifySendError: экспоненциальная пауза от номера попытки,
// исчерпание лимита попыток — failed (без капа вечный флап одной строки
// сжигал бы такты). Попытки считает ClaimDue, захват всегда даёт attempts ≥ 1.
func dbRetryBackoff(attempts, maxAttempts int, retryBase time.Duration) (wait time.Duration, giveUp bool) {
	if attempts < 1 {
		attempts = 1 // защита от отрицательного сдвига (классификатор ниже предполагает захваченную строку)
	}
	if attempts >= maxAttempts {
		return 0, true
	}
	return retryBase * (1 << (attempts - 1)), false
}

// handleSendError — классификация исхода отправки по политике §13.2.
func (l *DeliveryLoop) handleSendError(ctx context.Context, row OutboxItem, sendErr error) {
	message := sendErr.Error()
	decision := classifySendError(sendErr, row.Attempts, l.opts.MaxAttempts, l.opts.RetryBase)
	qctx, cancel := l.persistCtx()
	defer cancel()
	switch decision.kind {
	case outcomeRetryWait:
		if err := l.repo.MarkRetryWait(qctx, row.ID, time.Now().Add(decision.wait), message); err != nil {
			l.logger.Printf("delivery: retry_wait не записан: %v", err)
			return
		}
		l.logger.Printf("delivery: временный отказ — повтор после %v (turn %d, поз. %d): %s",
			decision.wait, row.TurnID, row.Position, message)
	case outcomeFailed:
		if err := l.repo.MarkFailed(qctx, row.ID, message); err != nil {
			l.logger.Printf("delivery: failed не записан: %v", err)
			return
		}
		l.logger.Printf("delivery: доставка провалилась навсегда (turn %d, поз. %d): %s",
			row.TurnID, row.Position, message)
	default: // outcomeUnknown
		_, cancelled, err := l.repo.MarkUnknown(qctx, row.ID, message)
		if err != nil {
			l.logger.Printf("delivery: unknown не записан: %v", err)
			return
		}
		l.logger.Printf("delivery: отправка неопределённа (unknown), зависимых отменено %d (turn %d, поз. %d): %s",
			cancelled, row.TurnID, row.Position, message)
	}
}

// outcomeKind — что делать с единицей доставки при ошибке (§13.2).
type outcomeKind int

const (
	outcomeRetryWait outcomeKind = iota // подтверждённый временный отказ
	outcomeFailed                       // подтверждённая постоянная ошибка
	outcomeUnknown                      // исход неизвестен: возможна отправка
)

type sendOutcome struct {
	kind outcomeKind
	wait time.Duration // для outcomeRetryWait
}

// classifySendError — чистая функция политики §13.2 (юнит-тестируется):
//   - 429 с retry_after — повтор после паузы (honor retry_after), но пауза
//     ограничена maxRetryAfterWait, а попытки — тем же потолком, что у 5xx (17.6);
//   - 5xx — повтор с экспоненциальной паузой до лимита попыток, дальше failed;
//   - прочие подтверждённые ошибки API (4xx) — постоянные: failed;
//   - всё, что не получило ответа (timeout, обрыв соединения, отмена) —
//     исход неизвестен: unknown, без автоповтора.
func classifySendError(err error, attempts, maxAttempts int, retryBase time.Duration) sendOutcome {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return sendOutcome{kind: outcomeUnknown}
	}
	if apiErr.HTTPStatus == 429 {
		// Тот же потолок попыток, что у 5xx (17.6): вечный флуд-контроль
		// по одной строке не должен жить в очереди бесконечно.
		if attempts >= maxAttempts {
			return sendOutcome{kind: outcomeFailed}
		}
		wait := apiErr.RetryAfter
		if wait <= 0 {
			wait = time.Second // паритет TS: retry_after не передан — 1 с
		}
		if wait > maxRetryAfterWait {
			wait = maxRetryAfterWait
		}
		return sendOutcome{kind: outcomeRetryWait, wait: wait}
	}
	if apiErr.HTTPStatus >= 500 {
		if attempts >= maxAttempts {
			return sendOutcome{kind: outcomeFailed}
		}
		return sendOutcome{kind: outcomeRetryWait, wait: retryBase * (1 << (attempts - 1))}
	}
	return sendOutcome{kind: outcomeFailed}
}

// chatHasNewPendingEvents — новые события чата ждут обработки (§13.3:
// устаревание перед отправкой).
func (l *DeliveryLoop) chatHasNewPendingEvents(ctx context.Context, chatID int64) (bool, error) {
	var exists bool
	err := l.db.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM social_events
		  WHERE chat_id = $1 AND status = 'pending' AND turn_id IS NULL
		)`, chatID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("delivery: новые события чата %d не проверяются: %w", chatID, err)
	}
	return exists, nil
}

// waitForBubbleGap держит паузу между bubbles одного turn (§13.3), в том
// числе когда пузыри приехали разными тактами.
func (l *DeliveryLoop) waitForBubbleGap(ctx context.Context, row OutboxItem) {
	if row.Position <= 0 {
		return
	}
	l.mu.Lock()
	last, ok := l.lastSent[row.TurnID]
	l.mu.Unlock()
	if !ok {
		return
	}
	wait := time.Until(last.Add(l.opts.BubbleGap))
	if wait <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

func (l *DeliveryLoop) noteSent(row OutboxItem) {
	l.mu.Lock()
	l.lastSent[row.TurnID] = time.Now()
	l.mu.Unlock()
}

// pruneLastSent чистит записи о bubbles завершившихся turn'ов.
func (l *DeliveryLoop) pruneLastSent() {
	cutoff := time.Now().Add(-10 * time.Minute)
	l.mu.Lock()
	for turnID, at := range l.lastSent {
		if at.Before(cutoff) {
			delete(l.lastSent, turnID)
		}
	}
	l.mu.Unlock()
}
