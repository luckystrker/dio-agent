// Проверка аватарки после разговорного хода (34.2): очередь фоновой задачи
// ставится дешёвой вставкой в БД, ответ не задерживается. Первый ход с
// человеком ставит проверку сразу; повторные — только когда суточное окно
// истекло (решение в internal/avatar.Enqueue).
package character

import (
	"context"

	"github.com/luckystrker/dio-agent/internal/avatar"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

// enqueueAvatarCheck ставит проверку аватарки первого актора пачки. Ошибка —
// в лог и не роняет применённый ход: это необязательная фоновая деталь.
func enqueueAvatarCheck(ctx context.Context, pool *runtime.Pool, in Input, logf func(string, ...any)) {
	if pool == nil {
		return
	}
	for _, ev := range in.Batch {
		if ev.ActorID == 0 {
			continue
		}
		if err := avatar.Enqueue(ctx, pool, ev.ActorID, in.ScopeID, in.ChatID, ev.ActorName); err != nil {
			logf("ход %d: проверка аватарки не поставлена: %v", in.TurnID, err)
		}
		return
	}
}
