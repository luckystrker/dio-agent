// Мягкий предохранитель частоты стикеров (28.1, решение 2026-11-03): живой
// прогон показал стикер в 6 из 8 ходов — каждый ответ превращался в реакцию,
// 3–4 сообщения и стикер. Стикер — редкая приправа, а не часть формы ответа.
// Модель предлагает (поле sticker), runtime решает: если стикер уже уходил в
// одном из последних ходов чата, предложение мягко обнуляется с логом, а ход
// живёт (PHILOSOPHY п.7). Предохранитель отключается нулём.
package character

import (
	"context"
	"fmt"
)

// stickerRateWindow — окно частоты: не больше одного стикера на столько
// последних ходов чата (включая текущий). 0 или 1 — предохранитель выключен.
const stickerRateWindow = 3

// stickerTooFrequent сообщает, уходил ли стикер в предыдущих ходах окна чата.
// Считаются только уже поставленные в outbox стикеры — то есть доставленные
// или ожидающие доставки ответы персонажа; чужие стикеры входящих событий не
// в счёт. DBTX: проверка нужна и при сборке контекста (38.1), и при применении.
func stickerTooFrequent(ctx context.Context, db DBTX, chatID, turnID int64) (bool, error) {
	if stickerRateWindow <= 1 {
		return false, nil
	}
	var recent int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM delivery_outbox o
		WHERE o.payload->>'kind' = 'sticker' AND o.turn_id IN (
			SELECT id FROM turns
			WHERE chat_id = $1 AND id <> $2
			ORDER BY id DESC LIMIT $3)`,
		chatID, turnID, stickerRateWindow-1).Scan(&recent); err != nil {
		return false, fmt.Errorf("стикер: частота не проверена: %w", err)
	}
	return recent > 0, nil
}
