# Ранбук: деплой dio-agent на VPS

Эксплуатационный минимум по §20 архитектуры: secrets вне репозитория,
systemd с автоперезапуском, миграции схемы, бэкап PostgreSQL, проверяемая
процедура восстановления, таймауты модели в конфигурации.

Ожидаемая машина: VPS с Ubuntu 22.04/24.04, 1–2 vCPU, 2 ГБ RAM, 20 ГБ диска.
Один процесс приложения; PostgreSQL на той же машине. Docker не обязателен.

## 1. Подготовка сервера

```bash
# от root
apt update && apt upgrade -y
apt install -y git curl

# Node.js 22 (проект требует >=22)
curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
apt install -y nodejs

# PostgreSQL 16 + pgvector из репозитория PGDG
apt install -y curl ca-certificates
install -d /usr/share/postgresql-common/pgdg
curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list
apt update
apt install -y postgresql-16 postgresql-16-pgvector
systemctl enable --now postgresql
```

Альтернатива: PostgreSQL можно не ставить нативно, а поднять тем же
`docker compose up -d` из репозитория (образ `pgvector/pgvector:pg17`) —
тогда на VPS нужен Docker, а пункт 2 пропускается.

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

## 3. Код и сборка (Linux)

```bash
adduser --system --group --home /opt/dio-agent dio
sudo -u dio git clone <репозиторий> /opt/dio-agent
cd /opt/dio-agent
sudo -u dio npm ci            # зависимости
sudo -u dio npm run build     # tsc → dist/
```

## 4. Переменные окружения

Файл `/opt/dio-agent/.env` (права 600, владелец dio; в git не попадает):

```ini
DATABASE_URL=postgres://dio:ПАРОЛЬ@127.0.0.1:5432/dio
NODE_ENV=production
LOG_LEVEL=info

# Модель (OpenAI-совместимый протокол). Стартовый endpoint — OpenCode Go,
# запасной — OpenRouter: переключение правкой этих строк, без redeploy кода.
MODEL_ENDPOINT=https://...
MODEL_API_KEY=...
MODEL_MODEL=...
# Таймауты/лимиты §17: 60 с на вызов, 1 повтор, 1000 токенов выхода,
# до 400 модельных вызовов в сутки.
# MODEL_TIMEOUT_MS=60000
# MODEL_MAX_RETRIES=1
# MODEL_MAX_OUTPUT_TOKENS=1000
# MODEL_DAILY_CALLS_LIMIT=400

# Telegram. Для первого запуска без токена: TELEGRAM_DRY_RUN=true
TELEGRAM_BOT_TOKEN=...
# TELEGRAM_API_BASE=https://api.telegram.org
```

Проверка конфигурации без запуска: `sudo -u dio npm run typecheck`.

## 5. Миграции

```bash
cd /opt/dio-agent && sudo -u dio npm run migrate
```

Миграции идемпотентны (node-pg-migrate, таблица `pgmigrations`).
Прогонять при каждом обновлении кода — шаг 9.

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
ExecStart=/usr/bin/node dist/index.js
Restart=always
RestartSec=5
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

Один процесс на базу гарантируется advisory-lock синглтоном (§5.1):
второй экземпляр при том же DATABASE_URL честно выйдет с кодом 3.

## 7. Проверка живости (smoke)

```bash
journalctl -u dio-agent -f
```

В логе старта ожидаем: `singleton захвачен`, `recovery очереди завершён`,
`запуск завершён; polling, worker, планировщик и доставка работают`.
Дальше каждые 15 с — пульс `жив` уровня debug (при LOG_LEVEL=info не виден)
и при непустой очереди — строка `очередь` с глубиной и возрастом старейшей
работы (§20).

Smoke без Telegram-токена: `TELEGRAM_DRY_RUN=true`, затем из второго
терминала `npm run feed -- "привет"` — updates читаются из файла, ответ
пишется в лог, ничего не уходит в Telegram.

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
journalctl -u dio-agent -n 50   # убедиться: singleton захвачен, recovery прошёл
```

Восстановление состояния после SIGKILL автоматическое (§5.3, §12.2):
незавершённая работа подбирается при старте, «зависшая» доставка помечается
unknown без слепой повторной отправки.

## 9. Обновление релиза

```bash
cd /opt/dio-agent
sudo -u dio git pull
sudo -u dio npm ci
sudo -u dio npm run build
sudo -u dio npm run migrate
systemctl restart dio-agent
journalctl -u dio-agent -f     # проконтролировать старт
```

Откат: `git checkout <предыдущий тег>`, повторить build+migrate+restart.
Миграции назад не откатываются (только вперёд) — при серьезной несовместимости
восстанавливай базу из бэкапа.

## 10. Наблюдаемость (§20)

- Журнал хода: каждая запись `ход выполнен` содержит turn/chat/work IDs,
  decision, число сообщений, latencyMs, usage токенов, бюджетную бронь
  (`budgetOperationId`) и reason. Полный приватный промпт не логируется.
- Терминальные пропуски: `работа помечена терминальным пропуском` (warn) с
  причиной — модель не настроена, бюджет исчерпан, лимит попыток.
- Доставка: `unknown`-отправки видны в статусах outbox и логах delivery.
- Очередь: глубина и возраст старейшей работы — в пульсе; при возрасте
  старше 120 с — warn `очередь растёт`.
- Поиск по журналу: `journalctl -u dio-agent --since "1 hour ago" | grep ход`.

## 11. Частые проблемы

| Симптом | Что смотреть |
|---|---|
| Процесс выходит с кодом 3 | Второй экземпляр с той же базой — синглтон (§5.1). Ищи дубль сервиса. |
| `работа помечена терминальным пропуском: модель не настроена` | Пусты MODEL_ENDPOINT/API_KEY/MODEL в `.env`. |
| `дневной бюджет модельных вызовов исчерпан` | Лимит MODEL_DAILY_CALLS_LIMIT; ходы возобновятся назавтра. |
| Ходы виснут в pending | `очередь` в пульсе, возраст старейшей работы; PostgreSQL жив (`systemctl status postgresql`). |
| Telegram не отвечает | Токен/доступы бота; dry-run выключен; `journalctl` на ошибки polling. |

## 12. Правка характера и системного промпта

Тексты персонажа лежат в двух файлах в корне проекта и правятся без изменения кода:

| Файл | Что внутри |
|---|---|
| `config/persona.md` | Личность: имя, описание, манера речи, ценности, базовое настроение (числа), биография-вымысел. |
| `config/system-prompt.md` | Каркас письма модели: метки {{persona}}, {{mood}}, {{relationship}}, {{memory}} заменяются живым состоянием при каждом ходе. |

Процедура:

```bash
nano config/persona.md        # правишь текст (правила — в комментарии внутри файла)
git add config/ && git commit -m "persona: ..." && git push   # правки переживают обновления релиза
# на VPS: git pull, затем
sudo systemctl restart dio-agent
journalctl -u dio-agent -n 20 | grep persona   # строка "persona загружена" с новой версией
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

Детали поведения: если секция в persona.md встретилась дважды, берётся первое
вхождение (дубль молча игнорируется — проверь файл, если правка «не применилась»).
Опечатка в метке шаблона обнаруживается при старте: процесс не поднимется,
в журнале будет имя метки.
