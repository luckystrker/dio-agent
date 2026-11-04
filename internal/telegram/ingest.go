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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luckystrker/dio-agent/internal/runtime"
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
func IngestUpdate(ctx context.Context, pool *runtime.Pool, raw json.RawMessage) (IngestResult, error) {
	return IngestUpdateWithBot(ctx, pool, raw, BotIdentity{})
}

// IngestUpdateWithBot — IngestUpdate с идентичностью бота: prefilter отличает
// reply/mention боту от переписки людей в группе (M8).
func IngestUpdateWithBot(ctx context.Context, pool *runtime.Pool, raw json.RawMessage, bot BotIdentity) (IngestResult, error) {
	event, err := NormalizeUpdateWithBot(raw, bot)
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
	// Правка (30.4): текст события — «было → стало», исходная строка получает
	// актуальную версию текста для истории чата.
	text := event.Text
	version := 0
	if event.Kind == KindEdited {
		// Повтор той же пачки update'ов не должен переписывать историю старой
		// версией текста: «было/стало» и обновление исходной строки — только для
		// новой записи.
		var duplicate bool
		if derr := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM social_events WHERE telegram_update_id = $1)`,
			event.TelegramUpdateID).Scan(&duplicate); derr != nil {
			return IngestResult{}, fmt.Errorf("ingest: дубль правки не проверен: %w", derr)
		}
		if !duplicate {
			text, version, err = applyEditedMessage(ctx, tx, event)
			if err != nil {
				return IngestResult{}, err
			}
		}
	}
	// occurred_at = время сообщения (message.date, у правки edit_date), не
	// приёма (18.1): после простоя накопленная пачка не должна выглядеть
	// свежей для границ эпизодов и проверок «чат ушёл вперёд». Даты нет — NULL,
	// колонка берёт now().
	var occurredAt *time.Time
	if event.Date > 0 {
		at := time.Unix(event.Date, 0).UTC()
		occurredAt = &at
	}
	// Реакция (34.1): цель ищем среди доставленных ответов бота (sent →
	// текст). Чужая цель — только история; группа без вовлечённости (§30.2) —
	// тоже история (бот получает такие реакции лишь админом).
	if event.Kind == KindReaction {
		quote, found, rerr := reactionTargetText(ctx, tx, event.ChatID, event.MessageID)
		if rerr != nil {
			return IngestResult{}, fmt.Errorf("ingest: цель реакции не найдена: %w", rerr)
		}
		reactionText := reactionEventText(event.Reaction, event.ReactionAdded, quote)
		text = &reactionText
		status = StatusJournalOnly
		if found {
			if event.ChatType == ChatPrivate {
				status = StatusPending
			} else {
				involved, ierr := GroupInvolved(ctx, tx, event.ChatID, time.Now())
				if ierr != nil {
					return IngestResult{}, fmt.Errorf("ingest: вовлечённость чата реакции не прочитана: %w", ierr)
				}
				if involved {
					status = StatusPending
				}
			}
		}
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO social_events
		  (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text,
		   raw_update, status, occurred_at, telegram_message_id, version, direct)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, coalesce($10::timestamptz, now()), $11, $12, $13)
		ON CONFLICT (telegram_update_id) DO NOTHING
		RETURNING id`,
		event.TelegramUpdateID, event.ChatID, string(event.ChatType),
		event.ActorID, event.ActorName, string(event.Kind), text,
		string(raw), string(status), occurredAt,
		event.MessageID, editVersionForInsert(event.Kind, version), event.Direct).Scan(&eventID)
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
func ReadOffset(ctx context.Context, pool *runtime.Pool) (int64, bool, error) {
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
