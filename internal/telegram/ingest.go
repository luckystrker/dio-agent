// Короткая транзакция ingest (§6.1): событие и следующий offset — атомарно.
// Offset двигается ТОЛЬКО здесь, и только после надёжной записи события (или
// подтверждения, что оно уже сохранено). Ingest ничего не ждёт: ни модели,
// ни буфера, ни доставки (инвариант 1). Поведение — src/telegram/ingest.ts.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PollOffsetKey — ключ poll offset в runtime_state (§18).
const PollOffsetKey = "telegram_offset"

// IngestResult — итог ingest одного update.
type IngestResult struct {
	// Inserted=false — событие уже сохранено ранее (дубль пачки), это не ошибка.
	Inserted bool
	// EventID — id строки social_events (для новой записи); 0 у дубля.
	EventID int64
	// Status — решение prefilter для новой записи.
	Status IngestStatus
	// Event — нормализованное событие (и для дубля: offset считается по нему).
	Event NormalizedEvent
}

// IngestUpdate сохраняет update одной короткой транзакцией (§6.1, шаги 2–5):
// вставка события с UNIQUE(telegram_update_id) (конфликт = «уже сохранено»),
// в той же транзакции — следующий poll offset. Ошибка БД откатывает всё:
// offset не двигается, пачка перепрочитается при retry.
func IngestUpdate(ctx context.Context, pool *pgxpool.Pool, raw json.RawMessage) (IngestResult, error) {
	event, err := NormalizeUpdate(raw)
	if err != nil {
		return IngestResult{}, err
	}
	status := DecideIngestStatus(event)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return IngestResult{}, fmt.Errorf("ingest: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var eventID int64
	inserted := true
	err = tx.QueryRow(ctx, `
		INSERT INTO social_events
		  (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (telegram_update_id) DO NOTHING
		RETURNING id`,
		event.TelegramUpdateID, event.ChatID, string(event.ChatType),
		event.ActorID, event.ActorName, string(event.Kind), event.Text,
		string(raw), string(status)).Scan(&eventID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return IngestResult{}, fmt.Errorf("ingest: событие не записано: %w", err)
		}
		inserted = false // дубль пачки: уже сохранено, не ошибка (§6.1, шаг 3)
	}
	if inserted {
		// Тот же offset в той же транзакции (§6.1, шаг 5); GREATEST — пачки
		// могут пересекаться, offset только растёт.
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime_state (key, value) VALUES ($1, to_jsonb($2::bigint))
			ON CONFLICT (key) DO UPDATE
			  SET value = GREATEST(runtime_state.value, EXCLUDED.value), updated_at = now()`,
			PollOffsetKey, event.TelegramUpdateID+1); err != nil {
			return IngestResult{}, fmt.Errorf("ingest: offset не сохранён: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return IngestResult{}, fmt.Errorf("ingest: транзакция не зафиксирована: %w", err)
	}
	return IngestResult{
		Inserted: inserted,
		EventID:  eventID,
		Status:   status,
		Event:    event,
	}, nil
}

// ReadOffset возвращает сохранённый poll offset (следующий ожидаемый
// update_id) и признак, что он уже сохранялся.
func ReadOffset(ctx context.Context, pool *pgxpool.Pool) (int64, bool, error) {
	var raw *string
	if err := pool.QueryRow(ctx,
		`SELECT value#>>'{}' FROM runtime_state WHERE key = $1`, PollOffsetKey).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("ingest: offset не читается: %w", err)
	}
	if raw == nil {
		return 0, false, nil
	}
	offset, err := strconv.ParseInt(*raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("ingest: offset не число: %q", *raw)
	}
	return offset, true, nil
}
