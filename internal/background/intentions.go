// Намерения (§14.1): единый механизм отложенных действий. Планировщик забирает
// due-намерения транзакционным CAS (pending → running, FOR UPDATE SKIP LOCKED) —
// двойная постановка невозможна; перед запуском проверяется актуальность
// (срок, дубль, «чат ушёл вперёд») и проактивная политика §14.3. Исполнение —
// executor.go как ago-задача (§12.2: чекпоинт + WAL).
package background

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// isNoRows отличает «строки нет» от прочих ошибок запроса.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// queryer — подмножество пула, достаточное чтениям репозитория.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Проактивная политика §14.3 (начальные настраиваемые ограничения спеки;
// паритет констант TS).
const (
	// MaxProactivePerChatPerDay — не более 2 инициативных последовательностей
	// на личный чат в сутки.
	MaxProactivePerChatPerDay = 2
	// MinProactiveGap — не менее 4 часов между инициативами.
	MinProactiveGap = 4 * time.Hour
	// MaxExplorationsPerDay — дневной лимит exploration (§16).
	MaxExplorationsPerDay = 2
)

// Виды намерений (§14.1).
const (
	KindReplyLater = "reply_later"
	KindCheckIn    = "check_in"
	KindShare      = "share"
	KindExplore    = "explore"
)

// initiativeKinds — виды, способные отправить сообщение вне активного ответа:
// учитываются в проактивных лимитах, если отправляются (§14.3).
func initiativeKinds() []string { return []string{KindReplyLater, KindCheckIn, KindShare} }

func isInitiativeKind(kind string) bool {
	for _, k := range initiativeKinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// Intention — строка intentions (поля, нужные исполнению; §14.1).
type Intention struct {
	ID            int64
	Kind          string
	TargetChatID  int64 // 0 допустим у explore (§14.1 targetChatId?)
	Topic         string
	Reason        string
	Status        string
	NotBefore     time.Time
	ExpiresAt     *time.Time
	RelatedTurnID *int64
	CreatedAt     time.Time
}

// proactiveUsage — сколько инициативных последовательностей чата «в деле»
// (доставки отправлены или ещё ждут отправки; отменённые/неудачные не в счёт)
// и когда была последняя (§14.3).
type proactiveUsage struct {
	Today int
	Last  *time.Time
}

// ProactiveAllowed — чистая политика §14.3: только личные чаты, не больше
// двух в сутки, не чаще раза в 4 часа. Группы выключены (isPrivate=false
// запрещает любую инициативу).
func ProactiveAllowed(isPrivate bool, usage proactiveUsage, now time.Time) (bool, string) {
	if !isPrivate {
		return false, "инициативы в группы выключены (§14.3)"
	}
	if usage.Today >= MaxProactivePerChatPerDay {
		return false, fmt.Sprintf("достигнут дневной лимит инициатив (%d/сутки, §14.3)", MaxProactivePerChatPerDay)
	}
	if usage.Last != nil && now.Sub(*usage.Last) < MinProactiveGap {
		return false, fmt.Sprintf("между инициативами прошло меньше %v (§14.3)", MinProactiveGap)
	}
	return true, ""
}

// IntentionRepo — работа с таблицей intentions.
type IntentionRepo struct {
	pool *runtime.Pool
}

// NewIntentionRepo строит репозиторий поверх пула.
func NewIntentionRepo(pool *runtime.Pool) *IntentionRepo {
	return &IntentionRepo{pool: pool}
}

const intentionCols = `id, kind, coalesce(target_chat_id, 0), topic, reason, status, not_before, expires_at, related_turn_id, created_at`

func scanIntention(row pgx.Row) (Intention, error) {
	var it Intention
	err := row.Scan(&it.ID, &it.Kind, &it.TargetChatID, &it.Topic, &it.Reason,
		&it.Status, &it.NotBefore, &it.ExpiresAt, &it.RelatedTurnID, &it.CreatedAt)
	if err != nil {
		return Intention{}, err
	}
	return it, nil
}

// ClaimDue атомарно переводит до limit due-намерений в running и возвращает их:
// срок пришёл, не истёк, ещё pending (§14.2). SKIP LOCKED — повторный такт или
// второй процесс не заберут то же намерение дважды. Проверка не вызывает
// модель (§14.2) — вызов модели делает executor у случайно взятых намерений.
func (r *IntentionRepo) ClaimDue(ctx context.Context, limit int) ([]Intention, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("намерения: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		UPDATE intentions SET status = 'running', updated_at = now()
		WHERE id IN (
		  SELECT id FROM intentions
		  WHERE status = 'pending' AND not_before <= now() AND expires_at > now()
		  ORDER BY priority DESC, not_before, id
		  LIMIT $1
		  FOR UPDATE SKIP LOCKED
		)
		RETURNING `+intentionCols, limit)
	if err != nil {
		return nil, fmt.Errorf("намерения: claim не удался: %w", err)
	}
	defer rows.Close()

	var out []Intention
	for rows.Next() {
		it, err := scanIntention(rows)
		if err != nil {
			return nil, fmt.Errorf("намерения: строка claim не читается: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("намерения: claim не дочитан: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("намерения: claim не зафиксирован: %w", err)
	}
	return out, nil
}

// Release возвращает исполнение в pending (перенос фона при прямом обращении
// §5.2, повтор после транзитного сбоя). notBefore — срок следующей попытки.
func (r *IntentionRepo) Release(ctx context.Context, id int64, notBefore time.Time) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE intentions SET status = 'pending', not_before = $2, updated_at = now()
		 WHERE id = $1 AND status = 'running'`, id, notBefore)
	if err != nil {
		return fmt.Errorf("намерения: возврат в pending не удался: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("намерения: %d не в running — возврат пропущен", id)
	}
	return nil
}

// Cancel отменяет намерение с причиной; терминальные статусы не трогает.
// Связанный ход инициативы (related_turn_id, ещё created) помечается skipped —
// иначе ход-сирота навсегда оседает created без событий (блок 18.7).
// Ложь (нечего отменять) — не ошибка: повторный гейт безвреден.
func (r *IntentionRepo) Cancel(ctx context.Context, id int64, reason string) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("намерения: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var related *int64
	err = tx.QueryRow(ctx,
		`UPDATE intentions SET status = 'cancelled', reason = $2, updated_at = now()
		 WHERE id = $1 AND status IN ('pending', 'running')
		 RETURNING related_turn_id`, id, reason).Scan(&related)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("намерения: отмена не удалась: %w", err)
	}
	if related != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE turns SET status = 'skipped' WHERE id = $1 AND status = 'created'`, *related); err != nil {
			return false, fmt.Errorf("намерения: ход-сирота %d не помечен: %w", *related, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("намерения: отмена не зафиксирована: %w", err)
	}
	return true, nil
}

// Fail помечает намерение неудачей (терминально; §14.1 статус failed) —
// например, ответ модели стабильно невалиден: иначе задача сжигала бы бюджет
// на каждом такте.
func (r *IntentionRepo) Fail(ctx context.Context, id int64, reason string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE intentions SET status = 'failed', reason = $2, updated_at = now()
		 WHERE id = $1 AND status IN ('pending', 'running')`, id, reason)
	if err != nil {
		return fmt.Errorf("намерения: fail не удался: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("намерения: %d не в исполняемом статусе — fail пропущен", id)
	}
	return nil
}

// ExpireOverdue отменяет просроченные pending-намерения (cleanup §14.2,
// «устаревшие планы получают expiry» §10.5): claim их всё равно не берёт.
// Связанные created-ходы помечаются skipped — ход-сирота не оседает вечно
// (блок 18.7).
func (r *IntentionRepo) ExpireOverdue(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("намерения: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		UPDATE intentions SET status = 'cancelled', reason = 'срок истёк (expiresAt)', updated_at = now()
		 WHERE status = 'pending' AND expires_at <= now()
		 RETURNING related_turn_id`)
	if err != nil {
		return 0, fmt.Errorf("намерения: истечение не применено: %w", err)
	}
	defer rows.Close()

	var orphans []int64
	cancelled := 0
	for rows.Next() {
		var related *int64
		if err := rows.Scan(&related); err != nil {
			return 0, fmt.Errorf("намерения: строка истечения не читается: %w", err)
		}
		cancelled++
		if related != nil {
			orphans = append(orphans, *related)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("намерения: истечение не дочитано: %w", err)
	}
	rows.Close()

	if len(orphans) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE turns SET status = 'skipped' WHERE id = ANY($1) AND status = 'created'`, orphans); err != nil {
			return 0, fmt.Errorf("намерения: ходы-сироты не помечены: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("намерения: истечение не зафиксировано: %w", err)
	}
	return cancelled, nil
}

// ResetRunning возвращает все running-намерения в pending — восстановление при
// старте процесса (§12.2: незавершённая фоновая задача продолжается с
// чекпоинта; singleton §5.1 означает, что running чужого живого процесса нет).
func (r *IntentionRepo) ResetRunning(ctx context.Context) (int, error) {
	tag, err := r.pool.Exec(ctx,
		`UPDATE intentions SET status = 'pending', updated_at = now() WHERE status = 'running'`)
	if err != nil {
		return 0, fmt.Errorf("намерения: сброс running не удался: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// --- гейты актуальности перед запуском (§14.1) -------------------------------

// ChatType возвращает тип чата по его событиям; событий нет — пусто (намерение
// в чат без истории не пишем: нечем проверить даже private).
func (r *IntentionRepo) ChatType(ctx context.Context, chatID int64) (string, error) {
	var ct *string
	err := r.pool.QueryRow(ctx,
		`SELECT chat_type FROM social_events WHERE chat_id = $1 ORDER BY id DESC LIMIT 1`,
		chatID).Scan(&ct)
	if err != nil {
		if isNoRows(err) {
			return "", nil
		}
		return "", fmt.Errorf("намерения: тип чата не читается: %w", err)
	}
	if ct == nil {
		return "", nil
	}
	return *ct, nil
}

// ChatMoved — «не закрылась ли тема / не был ли уже дан ответ» (§14.1): в чате
// появились события или отправленные доставки новее создания намерения —
// контекст ушёл вперёд, инициатива по старой теме неуместна.
func (r *IntentionRepo) ChatMoved(ctx context.Context, chatID int64, since time.Time) (bool, error) {
	moved, err := chatMovedQuery(ctx, r.pool, chatID, since)
	if err != nil {
		return false, fmt.Errorf("намерения: свежий контекст не проверяется: %w", err)
	}
	return moved, nil
}

// chatMovedQuery — тот же предикат для пула и транзакции применения: инициатива
// обязана перепроверить «чат ушёл вперёд» в той же транзакции, где ставится
// доставка (M1), иначе свежий ответ пользователя и отправка инициативы
// расходятся во времени.
func chatMovedQuery(ctx context.Context, q queryer, chatID int64, since time.Time) (bool, error) {
	var moved bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM social_events WHERE chat_id = $1 AND occurred_at > $2)
		    OR EXISTS (
		      SELECT 1 FROM delivery_outbox d
		      WHERE d.chat_id = $1 AND d.status = 'sent' AND d.created_at > $2)`,
		chatID, since).Scan(&moved)
	return moved, err
}

// DuplicatePending — «не появилось ли идентичное намерение» (§14.1): другое
// намерение того же вида в тот же чат по той же теме (после нормализации)
// ещё ждёт исполнения. Создание дубликатов блокирует UNIQUE(dedupe_key);
// этот гейт — второй пояс: он сравнивает СОДЕРЖИМОЕ (kind, чат, нормализованная
// тема), поэтому ловит и расхождение нормализации между запусками, при котором
// dedupe_key у двух строк разошёлся.
func (r *IntentionRepo) DuplicatePending(ctx context.Context, id int64, kind, topic string, chatID int64) (bool, error) {
	var dup bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM intentions
		  WHERE kind = $1
		    AND coalesce(target_chat_id, 0) = $2
		    AND lower(btrim(topic)) = lower(btrim($3))
		    AND id <> $4
		    AND status IN ('pending', 'running')
		)`,
		kind, chatID, topic, id).Scan(&dup)
	if err != nil {
		return false, fmt.Errorf("намерения: дубль не проверяется: %w", err)
	}
	return dup, nil
}

// ProactiveUsage считает использованный проактивный лимит чата: исполненные
// инициативные намерения, чей ход породил живые доставки (отправлены или ещё
// ждут), за сегодня и время последней (§14.3).
func (r *IntentionRepo) ProactiveUsage(ctx context.Context, chatID int64) (proactiveUsage, error) {
	var usage proactiveUsage
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE i.updated_at >= date_trunc('day', now())),
		       max(i.updated_at)
		FROM intentions i
		WHERE i.kind = ANY($1)
		  AND i.target_chat_id = $2
		  AND i.status = 'completed'
		  AND i.related_turn_id IS NOT NULL
		  AND EXISTS (
		    SELECT 1 FROM delivery_outbox d
		    WHERE d.turn_id = i.related_turn_id
		      AND d.status NOT IN ('cancelled', 'failed'))`,
		initiativeKinds(), chatID).Scan(&usage.Today, &usage.Last)
	if err != nil {
		return proactiveUsage{}, fmt.Errorf("намерения: проактивный лимит не читается: %w", err)
	}
	return usage, nil
}
