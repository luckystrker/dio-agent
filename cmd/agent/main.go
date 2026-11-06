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
	"strconv"
	"syscall"

	"github.com/luckystrker/dio-agent/internal/app"
	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/migrations"
)

// usage — обслуживающие подкоманды бинарника (этап 32.2). Без аргументов —
// обычный запуск агента.
const usage = `dio-agent — живой персонаж в Telegram.

Запуск без аргументов: полный жизненный цикл (§5.3).

Обслуживающие команды:
  entity-unmerge <id>   откатить объединение сущностей по журналу:
                        <id> — id поглощённой (невыжившей) сущности.
  replay <дамп.json>    проиграть сохранённый дамп модельного вызова с
                        абляциями (см. dio-agent replay --help).
`

func main() {
	if len(os.Args) > 1 {
		if err := runCommand(os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "команда не выполнена: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "запуск прерван: %v\n", err)
		os.Exit(1)
	}
}

func runCommand(args []string) error {
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "entity-unmerge":
		if len(args) != 2 {
			return fmt.Errorf("нужен ровно один аргумент: entity-unmerge <id>\n\n%s", usage)
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("id сущности должен быть положительным числом: %q", args[1])
		}
		return entityUnmerge(context.Background(), id)
	case "replay":
		return replay(args[1:])
	default:
		return fmt.Errorf("неизвестная команда %q\n\n%s", args[0], usage)
	}
}

// entityUnmerge откатывает склейку по журналу entity_merges: алиасы, связи
// памяти, stances с основаниями и отношения возвращаются на поглощённую
// сущность, merged_into снимается (этап 32.2).
func entityUnmerge(ctx context.Context, mergeID int64) error {
	if err := runtime.LoadDotEnv(".env"); err != nil {
		return err
	}
	cfg, err := runtime.LoadConfig(runtime.OSEnv)
	if err != nil {
		return err
	}
	if cfg.DatabaseURL == "" {
		return fmt.Errorf("пустой DATABASE_URL — откат читает журнал в базе")
	}
	pool, err := runtime.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return fmt.Errorf("PostgreSQL не подключён: %w", err)
	}
	defer pool.Close()
	// Схема могла быть обновлена новым бинарником, но старый сервис ещё не
	// перезапущен: миграции идемпотентны, CLI догоняет их сам.
	if _, err := runtime.Migrate(ctx, pool, migrations.FS); err != nil {
		return fmt.Errorf("миграции не применены: %w", err)
	}
	res, err := (memory.EntityRepo{}).Unmerge(ctx, pool, mergeID)
	if err != nil {
		return err
	}
	fmt.Printf("объединение отменено: поглощённая сущность %d снова отдельная; выжившая %d\n",
		res.MergeID, res.KeepID)
	return nil
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
