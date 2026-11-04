// Картинки outbox (этап 36.2): байты лежат рядом со строкой доставки, поэтому
// sendPhoto не ходит в сеть за картинкой повторно — ни на retry, ни после
// рестарта, и не зависит от того, что URL отдаёт сейчас.
package telegram

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Image — сохранённые байты картинки строки доставки.
func (r *OutboxRepo) Image(ctx context.Context, outboxID int64) (contentType string, data []byte, found bool, err error) {
	err = r.Pool.QueryRow(ctx,
		`SELECT content_type, bytes FROM outbox_images WHERE outbox_id = $1`, outboxID).
		Scan(&contentType, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("outbox: картинка строки %d не читается: %w", outboxID, err)
	}
	return contentType, data, true, nil
}
