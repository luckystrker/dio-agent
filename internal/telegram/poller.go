// Long polling (§6.1): updates обрабатываются в порядке update_id; offset
// хранится в БД и двигается только после записи события (§6.1, шаг 5). При
// ошибке (БД или API) offset не двигается, остаток пачки забирается заново
// после паузы: дубли отсекаются UNIQUE(telegram_update_id). Поведение —
// src/telegram/polling.ts.
package telegram

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PollerOptions — настройки polling.
type PollerOptions struct {
	// PollTimeout — long-poll таймаут getUpdates (25 с, паритет TS).
	PollTimeout time.Duration
	// ErrorBackoff — пауза после ошибки (3 с, паритет TS).
	ErrorBackoff time.Duration
}

func (o PollerOptions) withDefaults() PollerOptions {
	if o.PollTimeout <= 0 {
		o.PollTimeout = 25 * time.Second
	}
	if o.ErrorBackoff <= 0 {
		o.ErrorBackoff = 3 * time.Second
	}
	return o
}

// OnPendingEvent — приёмник нового pending-события (буфер чата).
type OnPendingEvent func(PendingEvent)

// TelegramPoller — цикл getUpdates → ingest → буферы.
type TelegramPoller struct {
	pool      *pgxpool.Pool
	api       API
	onPending OnPendingEvent
	logger    *log.Logger
	opts      PollerOptions
}

// NewTelegramPoller строит poller. pool — пул PostgreSQL; onPending —
// добавление события в буфер чата (nil — события только сохраняются).
func NewTelegramPoller(pool *pgxpool.Pool, api API, onPending OnPendingEvent, logger *log.Logger, opts PollerOptions) *TelegramPoller {
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
// старте, останавливается при shutdown). Остановка ошибкой не считается.
func (p *TelegramPoller) Run(ctx context.Context) error {
	offset, has, err := ReadOffset(ctx, p.pool)
	if err != nil {
		return err
	}
	if !has {
		offset = 0 // первый запуск: забираем всё, что Telegram ещё хранит
	}
	p.logger.Printf("polling запущен (offset %d)", offset)

	for {
		if ctx.Err() != nil {
			return nil
		}
		updates, err := p.api.GetUpdates(ctx, offset, int(p.opts.PollTimeout/time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			p.logger.Printf("polling: ошибка, повторю после %v: %v", p.opts.ErrorBackoff, err)
			if !p.backoff(ctx) {
				return nil
			}
			continue
		}

		// Пачка обрабатывается по порядку update_id (§6.1); первый сбой
		// прекращает пачку: offset не продвинут, retry заберёт остаток.
		failed := false
		for _, update := range updates {
			result, err := IngestUpdate(ctx, p.pool, update.Raw)
			if err != nil {
				if ctx.Err() != nil {
					return nil
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
		if failed && !p.backoff(ctx) {
			return nil
		}
	}
}

// backoff ждёт паузу после ошибки; false — контекст отменён.
func (p *TelegramPoller) backoff(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(p.opts.ErrorBackoff):
		return true
	}
}
