// Точка входа dio-agent (этап 9.1 — каркас).
//
// Порядок запуска (требование 9.1; полный startup по §5.3 — singleton,
// recovery, polling/worker/scheduler/delivery — приходят в 9.x):
//  1. конфигурация из окружения (с явной валидацией: невалидное значение —
//     причина не запускаться);
//  2. подключение к PostgreSQL (pgx);
//  3. применение миграций схемы §18 (идемпотентно);
//  4. ожидание сигнала завершения (SIGINT/SIGTERM) и graceful shutdown —
//     закрытие пула. Recovery обязан работать и после SIGKILL: graceful
//     shutdown не является условием корректности (§5.3).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "запуск прерван: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// .env — для локальной разработки; в проде переменные задаёт systemd
	// (RUNBOOK §6). Ошибки .env не молчим: сообщаем и останавливаемся.
	if err := runtime.LoadDotEnv(".env"); err != nil {
		return err
	}

	cfg, err := runtime.LoadConfig(runtime.OSEnv)
	if err != nil {
		return err
	}

	// Простой лог запуска; структурный логгер приходит с наблюдаемостью (§20, 9.6).
	logger := log.New(os.Stdout, "", log.LstdFlags)
	logger.Printf("dio-agent запускается: nodeEnv=%s logLevel=%s модель=%s telegram=%s",
		cfg.NodeEnv, cfg.LogLevel, modelState(cfg), telegramState(cfg))

	ctx := context.Background()

	pool, err := runtime.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.Print("PostgreSQL подключён")

	result, err := runtime.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		return err
	}
	logger.Printf("миграции: применено %d, пропущено %d", result.Applied, result.Skipped)

	// Ожидание сигнала: рабочие циклы (polling, worker, scheduler, delivery)
	// появятся в 9.x и будут останавливаться здесь же.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	sig := <-stop
	logger.Printf("получен сигнал %s — завершаюсь", sig)
	return nil
}

// modelState — человекочитаемое состояние модельного слоя для лога запуска.
func modelState(cfg runtime.Config) string {
	if cfg.Model == nil {
		return "не настроена"
	}
	return cfg.Model.Model
}

// telegramState — человекочитаемое состояние Telegram-слоя для лога запуска.
func telegramState(cfg runtime.Config) string {
	switch {
	case cfg.Telegram == nil:
		return "выключен"
	case cfg.Telegram.DryRun:
		return "dry-run"
	default:
		return "живой"
	}
}
