# Ранбук: деплой dio-agent на VPS

Эксплуатационный минимум по §20 архитектуры: secrets вне репозитория,
systemd с автоперезапуском, миграции схемы, бэкап PostgreSQL, проверяемая
процедура восстановления, таймауты модели в конфигурации.

Ожидаемая машина: VPS с Ubuntu 22.04/24.04, 1–2 vCPU, 2 ГБ RAM, 20 ГБ диска.
Один процесс приложения (Go-бинарник); PostgreSQL на той же машине. Docker
не обязателен.

## 1. Подготовка сервера

```bash
# от root
apt update && apt upgrade -y
apt install -y git curl

# Go (сборка на сервере; версия — см. go.mod в репозитории)
GO_VERSION=$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)
curl -fsSL "https://go.dev/dl/${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz
rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
ln -sf /usr/local/go/bin/go /usr/local/bin/go

# PostgreSQL 16 + pgvector из репозитория PGDG
apt install -y curl ca-certificates
install -d /usr/share/postgresql-common/pgdg
curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list
apt update
apt install -y postgresql-16 postgresql-16-pgvector
systemctl enable --now postgresql
```

Альтернатива сборке на сервере: собрать бинарник на рабочей машине и залить
его (шаг 3, вариант Б) — тогда Go на VPS не нужен, только git для обновления
файлов `config/`.

## 2. База данных

```bash
sudo -u postgres psql << 'SQL'
CREATE ROLE dio LOGIN PASSWORD 'СМЕНИ_ПАРОЛЬ';
CREATE DATABASE dio OWNER dio;
\c dio
CREATE EXTENSION vector;
SQL
```

Пароль придумай и сохрани в менеджере секретов — он пойдёт в `.env`.

## 3. Код и сборка Linux

Вариант А — собрать на сервере:

```bash
adduser --system --group --home /opt/dio-agent dio
sudo -u dio git clone <репозиторий> /opt/dio-agent
cd /opt/dio-agent
sudo -u dio go build -o dio-agent ./cmd/agent   # зависимости скачивает сам
```

Вариант Б — кросс-сборка на рабочей машине и заливка (Go — без CGO,
бинарник статический):

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dio-agent ./cmd/agent
scp dio-agent vps:/opt/dio-agent/dio-agent   # затем каждый релиз
```

На сервере обязательно должны лежать `config/persona.md` и
`config/system-prompt.md` — процесс читает их относительно рабочего
каталога (systemd `WorkingDirectory`).

## 4. Переменные окружения

Файл `/opt/dio-agent/.env` (права 600, владелец dio; в git не попадает).
Бинарник сам читает `.env` из рабочего каталога; переменные окружения
(например от systemd) сильнее файла:

```ini
DATABASE_URL=postgres://dio:ПАРОЛЬ@127.0.0.1:5432/dio
NODE_ENV=production
LOG_LEVEL=info

# Модель (OpenAI-совместимый протокол). Стартовый endpoint — OpenCode Go,
# запасной — OpenRouter: переключение правкой этих строк, без redeploy кода.
# Три переменные задаются вместе или ни одной; не настроены — ходы честно
# помечаются пропуском с причиной «модель не настроена».
MODEL_ENDPOINT=https://...
MODEL_API_KEY=...
MODEL_MODEL=...
# Таймауты/лимиты §17: 60 с на вызов, 1 повтор, 1000 токенов выхода,
# 100 модельных вызовов в сутки.
# MODEL_TIMEOUT_MS=60000
# MODEL_MAX_RETRIES=1
# MODEL_MAX_OUTPUT_TOKENS=1000
# MODEL_DAILY_CALLS_LIMIT=100
# Часовой пояс меток времени истории и панели состояния (IANA). Пусто —
# Europe/Moscow. Меняется без пересборки; неизвестная зона останавливает запуск.
# AGENT_TIMEZONE=Europe/Moscow
# Предзагрузка памяти (решение 2026-11-02): true — харнесс собирает карточку
# собеседника, и на целом разговоре инструменты памяти не выдаются; false —
# откат к принудительному memory_search на прямом ходе. Пусто — true.
# MODEL_MEMORY_PREFETCH=true

# Telegram. Для первого запуска без токена: TELEGRAM_DRY_RUN=true —
# отправки пишутся в лог, ничего не уходит в Telegram.
TELEGRAM_BOT_TOKEN=...
# TELEGRAM_API_BASE=https://api.telegram.org

# Векторизатор памяти (§10.4), OpenAI-совместимый endpoint; три переменные
# вместе или ни одной. Не настроен — семантический поиск выключен,
# full-text остаётся. Боевая настройка — neuraldeep, bge-m3; ключ — плейсхолдер.
# EMBEDDING_ENDPOINT=https://api.neuraldeep.ru/v1
# EMBEDDING_API_KEY=__PLACEHOLDER__
# EMBEDDING_MODEL=bge-m3
```

Валидация конфигурации строгая и происходит при старте до запуска циклов:
невалидное значение или противоречивая комбинация (например токен вместе с
dry-run) останавливают процесс с сообщением, называющим переменную.

## 5. Миграции

Отдельной команды нет: SQL-миграции из `migrations/` встроены в бинарник
и применяются при каждом старте (таблица `schema_migrations`, по порядку,
идемпотентно — уже применённые пропускаются). Строка старта
`миграции: применено N, пропущено M` показывает, что произошло.

## 6. systemd

`/etc/systemd/system/dio-agent.service`:

```ini
[Unit]
Description=dio-agent — живой Telegram-персонаж
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
Type=simple
User=dio
WorkingDirectory=/opt/dio-agent
EnvironmentFile=/opt/dio-agent/.env
ExecStart=/opt/dio-agent/dio-agent
Restart=always
RestartSec=5
# Graceful shutdown (§5.3) может до 90 с дожидаться текущего хода —
# запас, чтобы systemd не убил процесс посреди дренирования.
TimeoutStopSec=120
# Свободное место на диске — единственный лимит, который процесс реально ест
StandardOutput=journal
StandardError=journal
SyslogIdentifier=dio-agent

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now dio-agent
systemctl status dio-agent --no-pager
```

Один процесс на базу гарантируется advisory-lock синглтоном (§5.1): второй
экземпляр при том же DATABASE_URL сразу выходит с ненулевым кодом и
сообщением `singleton: замок занят`; systemd будет перезапускать его каждые
5 с, пока первый жив, — это ожидаемо, ищи дубль сервиса.

## 7. Проверка живости (smoke)

```bash
journalctl -u dio-agent -f
```

В логе старта ожидаем по порядку: `миграции: применено N, пропущено M`,
`singleton получен (pid N)`, `приложение запущено: модель=настроена
telegram=true`. Дальше каждые 15–30 с — пульс `heartbeat: фон жив: …`
уровня debug (при LOG_LEVEL=info не виден); при непустой очереди ходов в
нём же видны глубина и возраст старейшей (§20).

Ход в логе: `ход N выполнен: recovered=… памяти=… доставки=…`; доставка:
`delivery: отправлено (turn N, поз. N, message_id N)`.

Smoke без Telegram-токена: `TELEGRAM_DRY_RUN=true` — бот работает, отправки
пишутся в лог, ничего не уходит в Telegram.

## 8. Бэкап и восстановление PostgreSQL

Бэкап (cron, например nightly в 4:00, храним 14 дней):

```bash
# /etc/cron.d/dio-backup
0 4 * * * dio pg_dump -Fc dio > /var/backups/dio-$(date +\%F).dump && find /var/backups -name 'dio-*.dump' -mtime +14 -delete
```

Проверяемая процедура восстановления (прогнать хотя бы раз на тестовой
машине, §20 «проверяемая процедура»):

```bash
systemctl stop dio-agent
sudo -u postgres dropdb dio && sudo -u postgres createdb -O dio dio
sudo -u postgres pg_restore -d dio /var/backups/dio-ГГГГ-ММ-ДД.dump
sudo -u postgres psql -d dio -c 'CREATE EXTENSION IF NOT EXISTS vector;'
systemctl start dio-agent
journalctl -u dio-agent -n 50   # убедиться: миграции/singleton, recovery прошёл
```

Восстановление после сбоя процесса автоматическое (§5.3, §12.2, проверено
E2E 9.7): при старте незавершённые порции событий возвращаются в очередь,
ход с сохранённым результатом применяется без новой генерации, «зависшая»
доставка (sending мёртвого процесса) помечается unknown без слепой
повторной отправки. SIGKILL как условие корректности не требуется —
graceful не обязателен.

## 9. Обновление релиза

Вариант А (сборка на сервере):

```bash
cd /opt/dio-agent
sudo -u dio git pull
sudo -u dio go build -o dio-agent ./cmd/agent
systemctl restart dio-agent
journalctl -u dio-agent -f     # проконтролировать старт (миграции применит старт)
```

Вариант Б (кросс-сборка): собрать локально, `scp` бинарник, затем
`systemctl restart dio-agent`.

Откат: вернуть предыдущий бинарник (`git checkout <тег>` + пересборка или
прежний скопированный файл), перезапустить. Миграции назад не откатываются
(только вперёд) — при серьёзной несовместимости восстанавливай базу из
бэкапа.

## 10. Наблюдаемость (§20)

- Ход: `ход N выполнен: recovered=… памяти=… доставки=…`; фоновая задача —
  одна строка `фон: задача=… kind=… decision=… outcome=… latency_ms=…
  llm_calls=… tokens=… budget=… mood_reason=…`. Полный приватный промпт
  не логируется.
- Память (§10.4, этап 21): `ход N: память — инструменты вкл/выкл (причина:
  history_trimmed|card_incomplete|rollback|none), карточка: записей=… fits=…,
  семантика: …` — видно, почему ход пошёл в память инструментами. При старте:
  `семантика памяти: включена (модель …)` или `выключена (EMBEDDING_* не заданы)`.
- Терминальные пропуски: `ход N пропущен терминально: …` — модель не
  настроена, дневной бюджет исчерпан, лимит попыток.
- Очередь: глубина и возраст старейшего хода — в heartbeat; три такта
  подряд растущая очередь прямых обращений даёт warn `рост очереди прямых
  обращений: …` (§20), фон при этом откладывается.
- Доставка: `delivery: отправка неопределённа (unknown), зависимых отменено N`
  — обрыв без ответа; повторных слепых отправок нет.
- Поиск по журналу: `journalctl -u dio-agent --since "1 hour ago" | grep ход`.

## 11. Частые проблемы

| Симптом | Что смотреть |
|---|---|
| Выход с `singleton: замок занят` | Второй экземпляр с той же базой — синглтон (§5.1). Ищи дубль сервиса. |
| Выход при старте с именем переменной | Ошибка конфигурации: частично заданная группа MODEL_*/EMBEDDING_*, TELEGRAM_DRY_RUN вместе с токеном и т.п. Сообщение называет переменную. |
| `ход N пропущен терминально: модель не настроена` | Пусты MODEL_ENDPOINT/API_KEY/MODEL в `.env`. |
| `…: дневной бюджет … исчерпан` | Лимит MODEL_DAILY_CALLS_LIMIT; ходы пропускаются до конца суток. |
| Ходы виснут | Heartbeat: глубина/возраст очереди; warn `рост очереди…`; PostgreSQL жив (`systemctl status postgresql`). |
| Telegram не отвечает | Токен/доступы бота; dry-run выключен; `journalctl` на ошибки polling. |

## 12. Правка характера и системного промпта

Тексты персонажа лежат в двух файлах и правятся без изменения кода:

| Файл | Что внутри |
|---|---|
| `config/persona.md` | Личность: имя, описание, манера речи, ценности, базовое настроение (числа), биография-вымысел. |
| `config/system-prompt.md` | Каркас письма модели: только статика (персона, правила, примеры, контракт хода). Единственная живая метка — {{persona}}. |

Процедура:

```bash
nano config/persona.md        # правишь текст (правила — в комментарии внутри файла)
git add config/ && git commit -m "persona: ..." && git push   # правки переживают обновления релиза
# на VPS: git pull, затем
sudo systemctl restart dio-agent
journalctl -u dio-agent -n 20   # старт без ошибок = файл принят
```

Как это работает: при старте файл сравнивается с активной записью в базе.
Отличается — создаётся новая версия, прежние остаются в истории (таблица
`personas`): откат = вернуть старый текст файла и перезапустить. Ошибка в
файле (опечатка в заголовке секции, число вне диапазона, неизвестная метка
в шаблоне) останавливает запуск с сообщением, называющим проблемное место, —
исправь и запусти снова.

Что можно менять свободно: любой текст. Что нельзя: переименовывать заголовки
секций `## …` в persona.md и метки `{{…}}` в шаблоне. Комментарии `<!-- … -->`
в обоих файлах — подсказки для человека, в промпт они не попадают.

System-промпт обязан оставаться статичным: настроение, отношение, собеседник,
время и карточка памяти идут в служебную панель состояния, которую харнесс
добавляет в конец пользовательского сообщения (решение 2026-11-02). Метки
{{mood}}, {{relationship}}, {{actor}}, {{memory}} рендер по-прежнему понимает
для старых шаблонов, но в shipped-каркасе их нет: если добавить их обратно,
system начнёт меняться между ходами и кэш префикса провайдера сломается.

Детали поведения: если секция в persona.md встретилась дважды, берётся первое
вхождение (дубль молча игнорируется — проверь файл, если правка «не применилась»).
Опечатка в метке шаблона обнаруживается при старте: процесс не поднимется,
в журнале будет имя метки.

## Поведенческие эвалы (§21.2)

Офлайн (скриптовая модель, детерминированный гейт — гоняется в БД-тестах):

```bash
TEST_DATABASE_URL="postgres://dio:dio@localhost:5433/dio?sslmode=disable"   go test -tags "db,eval" ./internal/app/ -run TestEvalSuiteOffline
```

Живой (реальная модель; Telegram — заглушка; отчёт — `evals/results/live.json`):

```bash
TEST_DATABASE_URL="postgres://dio:dio@localhost:5433/dio?sslmode=disable"   MODEL_MODEL=nvidia/nemotron-3.5-super-vl:free   EVAL_MIN_PASS_RATE=0.5   go test -tags "db,eval,live" ./internal/app/ -run TestEvalSuiteLive -v -timeout 30m
```

Набор кейсов — `evals/behavior_suite.jsonl` (эталоны docs/BEHAVIOR_SCENARIOS.md);
одна строка — кейс: посев мира, реплики актора, критерии успеха. После правок
персоны/шаблона прогоняй живой набор и сравнивай pass_rate.

