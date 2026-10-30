// Delivery loop (§13): работает независимо от когнитивного worker, сохраняет
// порядок внутри чата и не блокирует cognition. Политика неопределённости
// (§13.2): подтверждённый временный отказ — retry_wait (уважая retry_after);
// подтверждённая постоянная ошибка — failed; timeout/обрыв после возможной
// отправки — unknown без автоповтора, зависимые пузыри отменяются. Перед
// каждой отправкой — проверка устаревания: новые события чата отменяют
// оставшиеся bubbles (§13.3). Поведение — src/telegram/delivery.ts.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	return o
}

// DeliveryLoop — отдельный цикл доставки из outbox.
type DeliveryLoop struct {
	db     DB
	repo   *OutboxRepo
	api    API
	logger *log.Logger
	opts   DeliveryOptions

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
		db:       db,
		repo:     repo,
		api:      api,
		logger:   logger,
		opts:     opts.withDefaults(),
		lastSent: map[int64]time.Time{},
	}
}

// Run крутит такты до отмены контекста (§5.3: delivery запускается на старте,
// останавливается при shutdown).
func (l *DeliveryLoop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.Tick(ctx); err != nil {
				l.logger.Printf("delivery: ошибка такта: %v", err)
			}
		}
	}
}

// Tick — один такт доставки (отдельным методом для тестируемости):
// захват пачки и отправка по порядку.
func (l *DeliveryLoop) Tick(ctx context.Context) error {
	rows, err := l.repo.ClaimDue(ctx, l.opts.BatchSize)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l.deliver(ctx, row)
	}
	l.pruneLastSent()
	return nil
}

// deliver отправляет одну единицу доставки с проверками §13.2/§13.3.
func (l *DeliveryLoop) deliver(ctx context.Context, row OutboxItem) {
	// Захваченная пачка могла устареть в памяти: предыдущий пузырь стал
	// unknown и отменил эту. Перепроверяем статус перед отправкой (§13.2).
	status, err := l.repo.StatusOf(ctx, row.ID)
	if err != nil {
		l.logger.Printf("delivery: статус строки %d не читается: %v", row.ID, err)
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
		l.logger.Printf("delivery: устаревание не проверяется (строка %d остаётся в sending до recovery): %v",
			row.ID, err)
		return
	}
	if stale && row.Position > 0 {
		cancelled, cerr := l.repo.CancelTurnFrom(ctx, row.TurnID, row.Position, "чат ушёл вперёд — пузыри устарели")
		if cerr != nil {
			l.logger.Printf("delivery: отмена устаревших пузырей turn %d не удалась: %v", row.TurnID, cerr)
			return
		}
		l.logger.Printf("delivery: доставка отменена — чат %d ушёл вперёд (turn %d, отменено %d)",
			row.ChatID, row.TurnID, cancelled)
		return
	}

	// §13.3: пауза между bubbles одного turn (не для первого сообщения).
	l.waitForBubbleGap(ctx, row)
	if ctx.Err() != nil {
		return
	}

	switch row.Kind {
	case "reaction":
		if row.TargetMessageID == 0 || row.Reaction == "" {
			_ = l.repo.MarkFailed(ctx, row.ID, "реакция без emoji/target_message_id")
			return
		}
		if err := l.api.SetMessageReaction(ctx, row.ChatID, row.TargetMessageID, row.Reaction); err != nil {
			l.handleSendError(ctx, row, err)
			return
		}
		// У реакции нет telegram_message_id — NULL, а не 0 (§13.1).
		if err := l.repo.MarkSent(ctx, row.ID, nil); err != nil {
			l.logger.Printf("delivery: sent реакции %d не записан: %v", row.ID, err)
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
		if err := l.repo.MarkSent(ctx, row.ID, &messageID); err != nil {
			l.logger.Printf("delivery: sent строки %d не записан: %v", row.ID, err)
			return
		}
		l.noteSent(row)
		l.logger.Printf("delivery: отправлено (turn %d, поз. %d, message_id %d)",
			row.TurnID, row.Position, messageID)
	}
}

// handleSendError — классификация исхода отправки по политике §13.2.
func (l *DeliveryLoop) handleSendError(ctx context.Context, row OutboxItem, sendErr error) {
	message := sendErr.Error()
	decision := classifySendError(sendErr, row.Attempts, l.opts.MaxAttempts, l.opts.RetryBase)
	switch decision.kind {
	case outcomeRetryWait:
		if err := l.repo.MarkRetryWait(ctx, row.ID, time.Now().Add(decision.wait), message); err != nil {
			l.logger.Printf("delivery: retry_wait не записан: %v", err)
			return
		}
		l.logger.Printf("delivery: временный отказ — повтор после %v (turn %d, поз. %d): %s",
			decision.wait, row.TurnID, row.Position, message)
	case outcomeFailed:
		if err := l.repo.MarkFailed(ctx, row.ID, message); err != nil {
			l.logger.Printf("delivery: failed не записан: %v", err)
			return
		}
		l.logger.Printf("delivery: доставка провалилась навсегда (turn %d, поз. %d): %s",
			row.TurnID, row.Position, message)
	default: // outcomeUnknown
		_, cancelled, err := l.repo.MarkUnknown(ctx, row.ID, message)
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
//   - 429 с retry_after — повтор после паузы (honor retry_after);
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
		wait := apiErr.RetryAfter
		if wait <= 0 {
			wait = time.Second // паритет TS: retry_after не передан — 1 с
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
