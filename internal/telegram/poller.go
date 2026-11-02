// Long polling (§6.1): updates обрабатываются в порядке update_id; offset
// хранится в БД и двигается только после записи события (§6.1, шаг 5). При
// транзиентной ошибке (БД или API) offset не двигается, остаток пачки
// забирается заново после паузы: дубли отсекаются UNIQUE(telegram_update_id).
// Классификация ошибок (16.4): 401/404 — токен отклонён, Run завершается
// ошибкой (честный останов под systemd вместо тихой глухоты с зелёным
// heartbeat); 409 — конфликт с другим потребителем getUpdates, пауза
// нарастает до потолка; update с неисправимыми данными (NUL в text/jsonb,
// отсутствие update_id) скипается с продвижением offset — ядовитый update
// не глушит приём. Поведение — src/telegram/polling.ts.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// PollerOptions — настройки polling.
type PollerOptions struct {
	// PollTimeout — long-poll таймаут getUpdates (25 с, паритет TS).
	PollTimeout time.Duration
	// ErrorBackoff — пауза после ошибки (3 с, паритет TS).
	ErrorBackoff time.Duration
	// ConflictBackoffCap — потолок паузы повтора при 409 (30 с, 16.4): пауза
	// конфликта нарастает от ErrorBackoff вдвое до этого предела.
	ConflictBackoffCap time.Duration
}

func (o PollerOptions) withDefaults() PollerOptions {
	if o.PollTimeout <= 0 {
		o.PollTimeout = 25 * time.Second
	}
	if o.ErrorBackoff <= 0 {
		o.ErrorBackoff = 3 * time.Second
	}
	if o.ConflictBackoffCap <= 0 {
		o.ConflictBackoffCap = 30 * time.Second
	}
	return o
}

// OnPendingEvent — приёмник нового pending-события (буфер чата).
type OnPendingEvent func(PendingEvent)

// TelegramPoller — цикл getUpdates → ingest → буферы.
type TelegramPoller struct {
	pool      *runtime.Pool
	api       API
	onPending OnPendingEvent
	logger    *log.Logger
	opts      PollerOptions
}

// NewTelegramPoller строит poller. pool — пул PostgreSQL; onPending —
// добавление события в буфер чата (nil — события только сохраняются).
func NewTelegramPoller(pool *runtime.Pool, api API, onPending OnPendingEvent, logger *log.Logger, opts PollerOptions) *TelegramPoller {
	if logger == nil {
		logger = log.New(discardWriter{}, "", 0)
	}
	return &TelegramPoller{
		pool:      pool,
		api:       api,
		onPending: onPending,
		logger:    logger,
		opts:      opts.withDefaults(),
	}
}

// Run крутит long polling до отмены контекста (§5.3: polling запускается на
// старте, останавливается при shutdown). Остановка ошибкой не считается,
// кроме 401/404 — отклонённый токен возвращается ошибкой: app делает
// fatalFail, под systemd это честный рестарт/останов, а не тихая глухота.
func (p *TelegramPoller) Run(ctx context.Context) error {
	offset, has, err := ReadOffset(ctx, p.pool)
	if err != nil {
		return err
	}
	if !has {
		offset = 0 // первый запуск: забираем всё, что Telegram ещё хранит
	}
	p.logger.Printf("polling запущен (offset %d)", offset)

	conflicts := 0 // подряд идущие 409: пауза конфликта нарастает (16.4)
	for {
		if ctx.Err() != nil {
			return nil
		}
		updates, err := p.api.GetUpdates(ctx, offset, int(p.opts.PollTimeout/time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) &&
				(apiErr.HTTPStatus == http.StatusUnauthorized || apiErr.HTTPStatus == http.StatusNotFound) {
				return fmt.Errorf("polling: Telegram отклонил токен (HTTP %d — %s), повтор бессмыслен: %w",
					apiErr.HTTPStatus, apiErr.Description, err)
			}
			wait := p.opts.ErrorBackoff
			if errors.As(err, &apiErr) && apiErr.HTTPStatus == http.StatusConflict {
				wait = conflictBackoff(p.opts, conflicts)
				conflicts++
			}
			p.logger.Printf("polling: ошибка, повторю после %v: %v", wait, err)
			if !p.sleep(ctx, wait) {
				return nil
			}
			continue
		}
		conflicts = 0 // успешный getUpdates гасит нарастание конфликта

		// Пачка обрабатывается по порядку update_id (§6.1); транзиентный сбой
		// прекращает пачку: offset не продвинут, retry заберёт остаток.
		// Неисправимые данные update'а повтором не лечатся — скип (16.4).
		failed := false
		for _, update := range updates {
			result, err := IngestUpdate(ctx, p.pool, update.Raw)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if unusableUpdateData(err) {
					// Ядовитый update: ERROR с идентификатором и скип. Offset
					// продвигается мимо в памяти; в БД его догоняет следующий
					// успешный ingest (GREATEST) — invariant «offset пишется
					// только в транзакции ingest» не нарушен. Повтор пачки
					// зависал бы на этом update навсегда.
					p.logger.Printf("polling: ERROR update %d неисправим, пропущен: %v", update.UpdateID, err)
					if next := update.UpdateID + 1; next > offset {
						offset = next
					}
					continue
				}
				p.logger.Printf("polling: update %d не сохранён — пачка повторится с offset %d после %v: %v",
					update.UpdateID, offset, p.opts.ErrorBackoff, err)
				failed = true
				break
			}
			offset = result.Event.TelegramUpdateID + 1
			if result.Inserted && result.Status == StatusPending && p.onPending != nil {
				p.onPending(PendingEvent{
					EventID:  result.EventID,
					ChatID:   result.Event.ChatID,
					ChatType: result.Event.ChatType,
					Kind:     result.Event.Kind,
					Text:     result.Event.Text,
				})
			}
		}
		if failed && !p.sleep(ctx, p.opts.ErrorBackoff) {
			return nil
		}
	}
}

// conflictBackoff — пауза повтора при 409 (конфликт с другим потребителем
// getUpdates, 16.4): нарастает от ErrorBackoff вдвое с каждым подряд идущим
// конфликтом до потолка ConflictBackoffCap. Без нарастания два конкурирующих
// потребителя молотили бы друг друга каждые 3 с бесконечно.
func conflictBackoff(opts PollerOptions, consecutive int) time.Duration {
	o := opts.withDefaults()
	wait := o.ErrorBackoff
	for i := 0; i < consecutive && wait < o.ConflictBackoffCap; i++ {
		wait *= 2
	}
	if wait > o.ConflictBackoffCap {
		wait = o.ConflictBackoffCap
	}
	return wait
}

// unusableUpdateData — ошибка данных update'а, не лечащаяся повтором (16.4):
// класс PostgreSQL 22xxx/23xxx (NUL в text/jsonb и другие нарушения формата
// или ограничений) либо ошибка нормализации (update без update_id).
// Транзиентные сбои БД (обрывы, 57xxx) сюда не попадают — пачка повторяется.
func unusableUpdateData(err error) bool {
	if errors.Is(err, ErrUnusableUpdate) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	class := pgErr.Code[:2]
	return class == "22" || class == "23"
}

// sleep ждёт паузу после ошибки; false — контекст отменён.
func (p *TelegramPoller) sleep(ctx context.Context, wait time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
		return true
	}
}
