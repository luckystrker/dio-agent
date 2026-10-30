# dio-agent — Living Telegram Agent

Постоянный AI-персонаж в Telegram: собеседник с характером, памятью, меняющимся
отношением к людям и миру. Спецификация — [docs/ARCHITECTURE_v0.7.md](docs/ARCHITECTURE_v0.7.md).

Go · PostgreSQL (+pgvector) · событийное ядро ago (`third_party/ago`) · один процесс (бинарник).

## Быстрый старт

Нужны Go (см. версию в `go.mod`) и Docker.

```bash
cp .env.example .env        # заполнить при необходимости (дефолты подходят для локальной разработки)
docker compose up -d        # PostgreSQL + pgvector
go run ./cmd/agent          # запуск: миграции применяются при старте, живой лог, Ctrl+C — graceful shutdown
```

Миграции встроены в бинарник и применяются автоматически при каждом старте
(`migrations/` → `embed`). Отдельной команды миграции нет.

Без токена Telegram: `TELEGRAM_DRY_RUN=true` — бот работает, отправки пишутся
в лог. Без `MODEL_*` — ходы честно помечаются пропуском с причиной.

Деплой на VPS — [docs/RUNBOOK.md](docs/RUNBOOK.md).

## Гейты качества

```bash
gofmt -l .          # пустой вывод
go build ./...
go vet ./...        # и go vet -tags db ./...
go test ./...       # юнит-тесты, живая БД не нужна
```

БД-тесты — отдельным прогоном против Postgres из docker-compose (вне гейта):

```bash
TEST_DATABASE_URL=postgres://dio:dio@localhost:5433/dio?sslmode=disable go test -tags db ./...
```

(Тот же гейт-набор перед коммитом гоняет хук `.zcode/hooks/pre-commit-gate.mjs`.)

## Документы

- `AGENTS.md` — индекс проекта для агентов: где что лежит и как работаем
- `CHECKLIST.md` — этапы разработки и их статус
- `docs/GLOSSARY.md` — словарь терминов (жаргон → по-русски)
- `docs/PHILOSOPHY.md` — приоритеты, с которыми сверяются решения
- `docs/RUNBOOK.md` — деплой на VPS и эксплуатация
