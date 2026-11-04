// Дневной лимит инициатив личного чата (§14.3, этап 33.2) — один расчёт для
// трёх мест: гейт фона перед исполнением намерения, панель состояния (модель
// видит, что планировать check_in/share уже нельзя) и применение хода (runtime
// не создаёт намерение, которое фон всё равно отменит). Дублировать SQL и
// порог нельзя: «достигнут дневной лимит» в логе фона и «больше нельзя» в
// панели обязаны означать одно и то же.
package character

import (
	"context"
	"fmt"
	"time"
)

// InitiativePerChatPerDay — не более двух инициативных последовательностей на
// личный чат в сутки (§14.3). Экспортируется: гейт фона и метрики берут то же
// число, что панель и применение.
const InitiativePerChatPerDay = 2

// InitiativeUsage — сколько инициативных последовательностей чата «в деле» за
// сегодня (доставки отправлены или ещё ждут) и время последней.
type InitiativeUsage struct {
	Today int
	Last  *time.Time
}

// IsInitiativeKind — виды, способные отправить сообщение вне активного ответа
// и потому считающиеся в проактивных лимитах, если отправляются (§14.3).
func IsInitiativeKind(kind string) bool {
	switch kind {
	case "reply_later", "check_in", "share":
		return true
	}
	return false
}

// InitiativeDailyExhausted — дневной лимит инициатив чата исчерпан (§14.3).
func InitiativeDailyExhausted(usage InitiativeUsage) bool {
	return usage.Today >= InitiativePerChatPerDay
}

// ReadInitiativeUsage считает израсходованный дневной лимит чата: исполненные
// инициативные намерения, чей ход породил живые доставки (отправлены или ещё
// ждут отправки; отменённые/неудачные не в счёт), за сегодня и время последней.
// Тот же запрос, что был в гейте фона (ProactiveUsage).
func ReadInitiativeUsage(ctx context.Context, db DBTX, chatID int64) (InitiativeUsage, error) {
	var usage InitiativeUsage
	err := db.QueryRow(ctx, `
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
		[]string{"reply_later", "check_in", "share"}, chatID).Scan(&usage.Today, &usage.Last)
	if err != nil {
		return InitiativeUsage{}, fmt.Errorf("намерения: проактивный лимит не читается: %w", err)
	}
	return usage, nil
}

// ChatTypeOf — тип чата по его последним событиям; событий нет — пусто
// (намерение в чат без истории не пишем: нечем проверить даже private).
// Общий запрос гейта фона и панели состояния.
func ChatTypeOf(ctx context.Context, db DBTX, chatID int64) (string, error) {
	var chatType *string
	err := db.QueryRow(ctx,
		`SELECT chat_type FROM social_events WHERE chat_id = $1 ORDER BY id DESC LIMIT 1`,
		chatID).Scan(&chatType)
	if err != nil {
		if isNoRows(err) {
			return "", nil
		}
		return "", fmt.Errorf("намерения: тип чата не читается: %w", err)
	}
	if chatType == nil {
		return "", nil
	}
	return *chatType, nil
}
