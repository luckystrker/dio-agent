// Outbox (§13.1): единицы доставки со статусами pending | sending | sent |
// retry_wait | unknown | cancelled | failed. UNIQUE(turn_id, position) —
// внутренний ключ против дублей намерений отправки; он не закрывает окно
// «Telegram принял запрос, ответ потерян» — exactly-once для внешнего API не
// заявляется (§13.2). Поведение — src/telegram/outbox.ts.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// OutboxStatus — статусы единицы доставки (§13.1).
type OutboxStatus string

const (
	OutboxPending   OutboxStatus = "pending"
	OutboxSending   OutboxStatus = "sending"
	OutboxSent      OutboxStatus = "sent"
	OutboxRetryWait OutboxStatus = "retry_wait"
	OutboxUnknown   OutboxStatus = "unknown"
	OutboxCancelled OutboxStatus = "cancelled"
	OutboxFailed    OutboxStatus = "failed"
)

// deliverableStatuses — статусы, из которых строку ещё можно отправить или
// отменить; sent/unknown/cancelled/failed терминальны.
const deliverableStatuses = `'pending', 'retry_wait', 'sending'`

// OutboxItem — единица доставки в памяти.
type OutboxItem struct {
	ID       int64
	TurnID   int64
	Position int
	ChatID   int64
	Kind     string // message | reaction
	Text     string
	Reaction string
	// TargetMessageID — цель реакции; 0 — не задана.
	TargetMessageID int64
	Status          OutboxStatus
	Attempts        int
}

// outboxPayload — формат delivery_outbox.payload (пишет character.ApplyTurn):
// {"kind":"message","text":…} | {"kind":"reaction","reaction":…,"target_message_id":…}.
type outboxPayload struct {
	Kind            string `json:"kind"`
	Text            string `json:"text"`
	Reaction        string `json:"reaction"`
	TargetMessageID int64  `json:"target_message_id"`
}

func decodePayload(raw []byte) outboxPayload {
	var p outboxPayload
	_ = json.Unmarshal(raw, &p) // битый payload даёт пустую структуру — не крашит доставку
	if p.Kind == "" {
		p.Kind = "message"
	}
	return p
}

// OutboxRepo — доступ к delivery_outbox (§13.1).
type OutboxRepo struct {
	Pool *runtime.Pool
}

// NewOutboxRepo строит репозиторий.
func NewOutboxRepo(pool *runtime.Pool) *OutboxRepo { return &OutboxRepo{Pool: pool} }

// ClaimDue захватывает к отправке pending/retry_wait с наступившим not_before
// — по порядку внутри чата (§13.1: chat_id, turn_id, position). Строка не
// берётся, если в том же чате есть более ранняя нетерминальная строка
// (pending/sending/retry_wait с меньшим turn_id/position, 17.7): без этого
// поздний пузырь обгонял ранний во флуд-паузе, и ответ читался навыворот.
// Блокировка не зависит от not_before ранней строки — «время ещё не пришло»
// не значит «уже можно обгонять». FOR UPDATE SKIP LOCKED: второй процесс не
// берёт чужие строки. Захваченное переводится в sending (перед сетью — §13.1),
// attempts растёт.
func (r *OutboxRepo) ClaimDue(ctx context.Context, limit int) ([]OutboxItem, error) {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("outbox: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ids []int64
	rows, err := tx.Query(ctx, `
		SELECT id FROM delivery_outbox o
		WHERE o.status IN ('pending', 'retry_wait') AND o.not_before <= now()
		  AND NOT EXISTS (
		    SELECT 1 FROM delivery_outbox prev
		    WHERE prev.chat_id = o.chat_id
		      AND prev.status IN (`+deliverableStatuses+`)
		      AND (prev.turn_id, prev.position) < (o.turn_id, o.position)
		  )
		ORDER BY o.chat_id ASC, o.turn_id ASC, o.position ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: очередь не читается: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("outbox: строка очереди не читается: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox: очередь не дочитана: %w", err)
	}
	if len(ids) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("outbox: транзакция не зафиксирована: %w", err)
		}
		return nil, nil
	}

	items, err := scanItems(ctx, tx, `
		UPDATE delivery_outbox
		SET status = 'sending', attempts = attempts + 1, updated_at = now()
		WHERE id = ANY($1::bigint[])
		RETURNING id, turn_id, position, chat_id, payload, status, attempts`, ids)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("outbox: транзакция не зафиксирована: %w", err)
	}
	// RETURNING отдаёт физический порядок вставки — сортируем явно:
	// порядок внутри чата обязателен (§13.1).
	sort.Slice(items, func(i, j int) bool { return deliveryOrderLess(items[i], items[j]) })
	return items, nil
}

// deliveryOrderLess — порядок доставки: чат, затем turn, затем позиция.
func deliveryOrderLess(a, b OutboxItem) bool {
	if a.ChatID != b.ChatID {
		return a.ChatID < b.ChatID
	}
	if a.TurnID != b.TurnID {
		return a.TurnID < b.TurnID
	}
	return a.Position < b.Position
}

func scanItems(ctx context.Context, q pgx.Tx, sql string, args ...any) ([]OutboxItem, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("outbox: строки не читаются: %w", err)
	}
	defer rows.Close()
	var items []OutboxItem
	for rows.Next() {
		var (
			item    OutboxItem
			payload []byte
		)
		if err := rows.Scan(&item.ID, &item.TurnID, &item.Position, &item.ChatID,
			&payload, &item.Status, &item.Attempts); err != nil {
			return nil, fmt.Errorf("outbox: строка не читается: %w", err)
		}
		p := decodePayload(payload)
		item.Kind, item.Text, item.Reaction, item.TargetMessageID = p.Kind, p.Text, p.Reaction, p.TargetMessageID
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox: строки не дочитаны: %w", err)
	}
	return items, nil
}

// MarkSent фиксирует подтверждённый успех: Telegram ID и sent (§13.1).
// Условная запись: строка, отменённая из устаревшей памяти, не перезаписывается.
// У реакции нет telegram_message_id — NULL (§13.1).
func (r *OutboxRepo) MarkSent(ctx context.Context, id int64, telegramMessageID *int64) error {
	tag, err := r.Pool.Exec(ctx, `
		UPDATE delivery_outbox
		SET status = 'sent', telegram_message_id = $2, last_error = NULL, updated_at = now()
		WHERE id = $1 AND status = 'sending'`, id, telegramMessageID)
	if err != nil {
		return fmt.Errorf("outbox: sent не записан: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("outbox: строка %d не в sending — sent не записан", id)
	}
	return nil
}

// StatusOf — текущий статус строки: перепроверка перед отправкой захваченной
// пачки (§13.2/§13.3: она могла устареть, пока лежала в памяти).
func (r *OutboxRepo) StatusOf(ctx context.Context, id int64) (OutboxStatus, error) {
	var status OutboxStatus
	if err := r.Pool.QueryRow(ctx,
		`SELECT status FROM delivery_outbox WHERE id = $1`, id).Scan(&status); err != nil {
		return "", fmt.Errorf("outbox: статус строки %d не читается: %w", id, err)
	}
	return status, nil
}

// MarkRetryWait — подтверждённый временный отказ: пауза до повторной попытки,
// для 429 — с honor retry_after (§13.2).
func (r *OutboxRepo) MarkRetryWait(ctx context.Context, id int64, notBefore time.Time, sendErr string) error {
	tag, err := r.Pool.Exec(ctx, `
		UPDATE delivery_outbox
		SET status = 'retry_wait', not_before = $2, last_error = $3, updated_at = now()
		WHERE id = $1 AND status = 'sending'`, id, notBefore, sendErr)
	if err != nil {
		return fmt.Errorf("outbox: retry_wait не записан: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("outbox: строка %d не в sending — retry_wait не записан", id)
	}
	return nil
}

// RequeueFromSending — ошибка БД до попытки отправки (§13.2, этап 16.3):
// захваченная строка возвращается из sending в retry_wait, чтобы не зависать
// до рестарта (ClaimDue берёт только pending/retry_wait). Условная запись —
// как MarkSent: терминальный исход, случившийся пока ошибка шла до записи,
// не перезаписывается.
func (r *OutboxRepo) RequeueFromSending(ctx context.Context, id int64, notBefore time.Time, sendErr string) error {
	tag, err := r.Pool.Exec(ctx, `
		UPDATE delivery_outbox
		SET status = 'retry_wait', not_before = $2, last_error = $3, updated_at = now()
		WHERE id = $1 AND status = 'sending'`, id, notBefore, sendErr)
	if err != nil {
		return fmt.Errorf("outbox: retry_wait не записан: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("outbox: строка %d не в sending — retry_wait не записан", id)
	}
	return nil
}

// MarkFailed — подтверждённая постоянная ошибка (§13.2). Условная запись, как
// MarkSent: терминальный исход, записанный recovery, не перезаписывается.
func (r *OutboxRepo) MarkFailed(ctx context.Context, id int64, sendErr string) error {
	tag, err := r.Pool.Exec(ctx, `
		UPDATE delivery_outbox SET status = 'failed', last_error = $2, updated_at = now()
		WHERE id = $1 AND status = 'sending'`, id, sendErr)
	if err != nil {
		return fmt.Errorf("outbox: failed не записан: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("outbox: строка %d не в sending — failed не записан", id)
	}
	return nil
}

// MarkUnknown — неопределённая отправка (§13.2): timeout/обрыв после возможной
// отправки. Автоматически НЕ повторяется; оставшиеся зависимые пузыри той же
// последовательности отменяются — компромисс в пользу отсутствия дублей.
// Переводит в unknown только строку в sending: у терминальной строки исхода
// «неизвестно» быть не может (changed=false — уже терминальна, честный no-op).
// Возвращает changed и число отменённых зависимых.
func (r *OutboxRepo) MarkUnknown(ctx context.Context, id int64, sendErr string) (changed bool, cancelled int, err error) {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("outbox: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var turnID int64
	var position int
	err = tx.QueryRow(ctx, `
		UPDATE delivery_outbox SET status = 'unknown', last_error = $2, updated_at = now()
		WHERE id = $1 AND status = 'sending'
		RETURNING turn_id, position`, id, sendErr).Scan(&turnID, &position)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Строка уже терминальна (например, отменена восстановлением
			// соседнего зависшего sending): честный no-op.
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("outbox: unknown не записан: %w", err)
	}
	cancelled, err = cancelTurnFromTx(ctx, tx, turnID, position+1, "отменено из-за unknown в последовательности")
	if err != nil {
		return false, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, 0, fmt.Errorf("outbox: транзакция не зафиксирована: %w", err)
	}
	return true, cancelled, nil
}

// CancelTurnFrom отменяет ещё не отправленные пузыри turn, начиная с позиции
// fromPosition (§13.3: оставшиеся bubbles устаревшей последовательности).
func (r *OutboxRepo) CancelTurnFrom(ctx context.Context, turnID int64, fromPosition int, reason string) (int, error) {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("outbox: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	cancelled, err := cancelTurnFromTx(ctx, tx, turnID, fromPosition, reason)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("outbox: транзакция не зафиксирована: %w", err)
	}
	return cancelled, nil
}

func cancelTurnFromTx(ctx context.Context, tx pgx.Tx, turnID int64, fromPosition int, reason string) (int, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE delivery_outbox SET status = 'cancelled', last_error = $3, updated_at = now()
		WHERE turn_id = $1 AND position >= $2 AND status IN (`+deliverableStatuses+`)`,
		turnID, fromPosition, reason)
	if err != nil {
		return 0, fmt.Errorf("outbox: отмена пузырей не удалась: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RecoverStaleSending — recovery (§12.2 «Outbox sending старого процесса —
// в unknown, не отправлять вслепую»): sending, который висит дольше stale,
// переводится в unknown, зависимые пузыри отменяются. Возвращает число
// затронутых строк.
func (r *OutboxRepo) RecoverStaleSending(ctx context.Context, stale time.Duration) (int, error) {
	rows, err := r.Pool.Query(ctx, `
		SELECT id FROM delivery_outbox
		WHERE status = 'sending' AND updated_at < now() - $1::interval`,
		stale)
	if err != nil {
		return 0, fmt.Errorf("outbox: sending не читается: %w", err)
	}
	defer rows.Close()
	var staleIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("outbox: строка sending не читается: %w", err)
		}
		staleIDs = append(staleIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("outbox: sending не дочитан: %w", err)
	}
	touched := 0
	for _, id := range staleIDs {
		changed, cancelled, err := r.MarkUnknown(ctx, id, "sending завис после сбоя процесса (§12.2)")
		if err != nil {
			return touched, fmt.Errorf("outbox: recovery строки %d не удался: %w", id, err)
		}
		if changed {
			touched += 1 + cancelled
		}
	}
	return touched, nil
}
