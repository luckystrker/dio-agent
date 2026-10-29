# dio-agent — Living Telegram Agent

Постоянный AI-персонаж в Telegram: собеседник с характером, памятью, меняющимся
отношением к людям и миру. Спецификация — [docs/ARCHITECTURE_v0.7.md](docs/ARCHITECTURE_v0.7.md).

> Идёт переезд с TypeScript на Go (этап 9 CHECKLIST.md): команды ниже описывают TS-версию
> до завершения переезда; Go-версия собирается `go build ./cmd/agent`.

## Быстрый старт

```bash
cp .env.example .env        # заполнить при необходимости (дефолты подходят для локальной разработки)
docker compose up -d        # PostgreSQL + pgvector
npm install
npm run migrate             # применить миграции
npm run dev                 # запуск: живой лог, heartbeat каждые 15 с, Ctrl+C — graceful shutdown
```

## Команды

| Команда | Что делает |
|---|---|
| `npm run dev` | Запуск в режиме разработки (tsx watch) |
| `npm run build` / `npm start` | Сборка в `dist/` и запуск собранного |
| `npm run lint` / `typecheck` / `test` | Гейты качества (те же гоняет хук перед коммитом) |
| `npm run migrate` | Применить миграции БД |
| `npm run migrate:create <имя>` | Создать файл новой миграции |

## Документы

- `AGENTS.md` — индекс проекта для агентов: где что лежит и как работаем
- `CHECKLIST.md` — этапы разработки и их статус
- `docs/GLOSSARY.md` — словарь терминов (жаргон → по-русски)
- `docs/PHILOSOPHY.md` — приоритеты, с которыми сверяются решения
