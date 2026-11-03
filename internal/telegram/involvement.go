// Вовлечённость группы (30.2): состояние в БД, переживает рестарт.
//
// После ответа/реакции бота involved_until = now+5m, ceiling_until = now+30m
// (если потолок ещё жив — он не продлевается: 30 минут подряд без прямого
// обращения — предел). Пока окно живо, каждое сообщение продлевает его, но не
// дальше потолка. Прямое обращение к боту сбрасывает потолок.
//
// last_unprompted_at — потолок «встреваний» без прямого обращения (30.1):
// не чаще раза в 20 минут на группу.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// rowQuerier — только чтение одной строки (сборке контекста хода не нужен
// Exec).
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ResetGroupCeiling — прямое обращение к боту: потолок вовлечённости заново
// 30 минут (окно вовлечённости начнётся, когда бот ответит).
func ResetGroupCeiling(ctx context.Context, db groupDB, chatID int64, now time.Time) error {
	_, err := db.Exec(ctx, `
		INSERT INTO chat_involvement (chat_id, ceiling_until, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (chat_id) DO UPDATE
		  SET ceiling_until = EXCLUDED.ceiling_until, updated_at = now()`,
		chatID, now.Add(GroupInvolvementCeiling))
	if err != nil {
		return fmt.Errorf("вовлечённость: потолок чата %d не сброшен: %w", chatID, err)
	}
	return nil
}

// ExtendGroupInvolvement — сообщение в вовлечённом чате продлевает окно на
// 5 минут, но не дальше потолка. Не вовлечён — ничего не меняется.
func ExtendGroupInvolvement(ctx context.Context, db groupDB, chatID int64, now time.Time) error {
	_, err := db.Exec(ctx, `
		UPDATE chat_involvement
		SET involved_until = LEAST($2::timestamptz, ceiling_until), updated_at = now()
		WHERE chat_id = $1
		  AND involved_until IS NOT NULL AND involved_until > $3
		  AND ceiling_until IS NOT NULL AND ceiling_until > $3`,
		chatID, now.Add(GroupInvolvementWindow), now)
	if err != nil {
		return fmt.Errorf("вовлечённость: окно чата %d не продлено: %w", chatID, err)
	}
	return nil
}

// InvolveAfterBotAction — бот ответил/поставил реакцию/отправил стикер в
// группе: чат вовлечён. Потолок заводится заново, только если прежний истёк
// (подряд идущая вовлечённость не продлевается сверх 30 минут).
func InvolveAfterBotAction(ctx context.Context, db groupDB, chatID int64, now time.Time) error {
	_, err := db.Exec(ctx, `
		INSERT INTO chat_involvement (chat_id, involved_until, ceiling_until, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (chat_id) DO UPDATE
		  SET ceiling_until = CASE
		        WHEN chat_involvement.ceiling_until IS NULL
		          OR chat_involvement.ceiling_until <= $4 THEN $3
		        ELSE chat_involvement.ceiling_until END,
		      involved_until = LEAST($2, CASE
		        WHEN chat_involvement.ceiling_until IS NULL
		          OR chat_involvement.ceiling_until <= $4 THEN $3
		        ELSE chat_involvement.ceiling_until END),
		      updated_at = now()`,
		chatID, now.Add(GroupInvolvementWindow), now.Add(GroupInvolvementCeiling), now)
	if err != nil {
		return fmt.Errorf("вовлечённость: ответ бота в чате %d не отмечен: %w", chatID, err)
	}
	return nil
}

// GroupInvolved — чат сейчас вовлечён (окно живо и потолок не истёк).
func GroupInvolved(ctx context.Context, db rowQuerier, chatID int64, now time.Time) (bool, error) {
	var involved bool
	err := db.QueryRow(ctx, `
		SELECT coalesce(involved_until > $2 AND ceiling_until > $2, false)
		FROM chat_involvement WHERE chat_id = $1`, chatID, now).Scan(&involved)
	if err != nil {
		if isNoRowsError(err) {
			return false, nil
		}
		return false, fmt.Errorf("вовлечённость: чат %d не читается: %w", chatID, err)
	}
	return involved, nil
}

// GroupUnpromptedAllowed — потолок встреваний без прямого обращения: true,
// если прошло не меньше GroupUnpromptedCooldown с последнего такого хода.
func GroupUnpromptedAllowed(ctx context.Context, db rowQuerier, chatID int64, now time.Time) (bool, error) {
	var allowed bool
	err := db.QueryRow(ctx, `
		SELECT coalesce(last_unprompted_at IS NULL OR last_unprompted_at <= $2, true)
		FROM chat_involvement WHERE chat_id = $1`, chatID, now.Add(-GroupUnpromptedCooldown)).Scan(&allowed)
	if err != nil {
		if isNoRowsError(err) {
			return true, nil
		}
		return false, fmt.Errorf("вовлечённость: потолок встреваний чата %d не читается: %w", chatID, err)
	}
	return allowed, nil
}

// TouchGroupUnprompted — отметка встревания без прямого обращения.
func TouchGroupUnprompted(ctx context.Context, db groupDB, chatID int64, now time.Time) error {
	_, err := db.Exec(ctx, `
		INSERT INTO chat_involvement (chat_id, last_unprompted_at, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (chat_id) DO UPDATE
		  SET last_unprompted_at = EXCLUDED.last_unprompted_at, updated_at = now()`,
		chatID, now)
	if err != nil {
		return fmt.Errorf("вовлечённость: встречание чата %d не отмечено: %w", chatID, err)
	}
	return nil
}

// isNoRowsError — «строки нет» (чат ещё ни разу не вовлекался).
func isNoRowsError(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
