// Точка входа dio-agent: полный запуск по §5.3.
//
// Порядок запуска (внутри app.Start):
//  1. конфигурация из окружения (с явной валидацией: невалидное значение —
//     причина не запускаться);
//  2. подключение к PostgreSQL и миграции схемы §18;
//  3. singleton (session advisory lock, §5.1): занят — процесс не запускается;
//     потеря соединения останавливает процесс под перезапуск systemd;
//  4. персона из config/persona.md (+ версии в БД §7.1);
//  5. recovery незавершённой работы (§12.2);
//  6. polling, когнитивный worker, планировщик фона, доставка.
//
// Завершение (SIGINT/SIGTERM): graceful shutdown §5.3 — остановить приём,
// ограниченно дождаться текущего хода, закрыть соединения. Recovery обязан
// работать и после SIGKILL: graceful shutdown не является условием
// корректности.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/luckystrker/dio-agent/internal/app"
	"github.com/luckystrker/dio-agent/internal/runtime"
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

	// Сигналы → graceful shutdown (§5.3); занятый singleton и потеря замка
	// возвращают ошибку — ненулевой код выхода под restart systemd.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return app.Run(ctx, app.Options{Config: cfg})
}
