//go:build db

// БД-тест проводки dry-run Telegram из App.Start (16.6, мутационная находка
// приёмки М2): сборка приложения обязана передать заглушке логгер приложения —
// доставка dry-run видна в журнале процесса с форматом/направлением a.logger,
// а не в дефолтном stderr-логгере внутри заглушки.
package app

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// syncBuffer — потокобезопасный буфер логгера: delivery loop пишет из своей
// горутины, тест читает из своей.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAppStartPassesLoggerToDryRunAPI — ТЗ 16.6 «сборка приложения передаёт
// непустой логгер»: App.Start собирает dry-run API (opts.Telegram=nil,
// TELEGRAM_DRY_RUN=true), доставка идёт через него, отправка доходит до
// логгера приложения.
func TestAppStartPassesLoggerToDryRunAPI(t *testing.T) {
	dsn := newTestDB(t)

	var buf syncBuffer
	h := newHarnessDSNTuned(t, dsn, func(o *Options) {
		o.Telegram = nil // путь сборки: buildTelegramAPI вызывается из Start
		o.Config.Telegram = &runtime.TelegramSettings{DryRun: true}
		o.Logger = log.New(&buf, "dio: ", 0)
	})

	// Пузырь доставки: delivery loop отправит его через собранный Start'ом API.
	if _, err := h.pool.Exec(context.Background(), `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, attempts)
		VALUES (1, 0, $1, '{"kind":"message","text":"сухой прогон"}'::jsonb, 'pending', 0)`,
		lcChat); err != nil {
		t.Fatalf("пузырь доставки не посеян: %v", err)
	}

	eventually(t, "отправка dry-run дошла до логгера приложения", func() bool {
		return strings.Contains(buf.String(), "dry-run: sendMessage")
	})
}
