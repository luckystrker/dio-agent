# Ревью этапа 42.1 — whitelist лички (коммит 5b5c630)

Дата: 2026-11-07. Ревьюер: независимый, только чтение (код не менялся, ничего не коммитилось).
Объём: `git show 5b5c630` — 12 файлов, +452/−5. Проверены также смежники вне диффа:
`internal/telegram/{api,delivery,buffers,prefilter,edits,blocked}.go`,
`internal/app/app.go`, `internal/character/{middleware,apply,turn,avatar_wiring}.go`,
`internal/background/{boredom,executor,life_events,avatar,group_share}.go`,
`internal/worker/worker.go`, `internal/avatar/avatar.go`.

Вывод коротко: вход (ingest→journal_only, offset, дубли, правки/реакции) и выход
(GuardAPI по всем 6 методам API, 403→failed+blocked, стартовый `BlockForeignPrivateChats`,
гейты фона/применения) — корректны, группы не затронуты. Но есть 2 MAJOR (один —
реальный обход запрета на typing, второй — залипание блокировки при возврате id
в список) и 5 MINOR. Чистого «всё чисто» нет.

## MAJOR

### M1. Typing-пульс построен от не обёрнутого API — typing уходит в чужую личку

- Файлы: `internal/app/app.go:361-365` (создание пульса) против `:413-420` (обёртка GuardAPI).
- Сценарий отказа: `api := a.opts.Telegram / buildTelegramAPI(...)` → на строке 363
  `pulse = character.NewTypingPulse(api, ...)` захватывает **сырой** клиент; только потом,
  на строке 420, `api = telegram.GuardAPI(api, allow)` оборачивает локальную переменную,
  а `runner.Typing` продолжает указывать на пульс с сырым отправителем.
  `TypingMiddleware` (`internal/character/middleware.go:242`) шлёт `SendChatAction(typing)`
  напрямую через него, минуя `deny`. Любой ход по чужой личке (остаточный pending-turn,
  созданный до включения whitelist, или ход по намерению, проскочившему гейты, см. M3)
  покажет «печатает…» чужому — ровно то, что этап запрещает («ни … typing»).
- В установившемся режиме новых ходов по чужим личкам нет (journal_only не доходит до
  буфера, `poller.go:159`), поэтому это не BLOCKER, но обход детерминированный, а не
  теоретический: достаточно одной остаточной строки в `turns`/буфере.
- Фикс: создавать пульс после обёртки (перенести блок 361–365 ниже строки 430) либо
  обернуть отправитель: `character.NewTypingPulse(telegram.GuardAPI(api, allow), ...)`.
  Дешёвый юнит-тест: пульс над GuardAPI для чужого id не должен делать HTTP-вызовов.

### M2. Возврат id в allowlist не снимает залипшую пометку blocked_chats

- Файлы: `internal/telegram/allowlist.go:120-157` (`BlockForeignPrivateChats` только
  INSERT'ит, DELETE нет); `internal/background/executor.go:581-585`,
  `internal/background/boredom.go:225-240`, `internal/character/apply.go:759-765` (все
  гейты смотрят на `blocked_chats`).
- Сценарий отказа:
  1. id `X` убрали из `TELEGRAM_PRIVATE_ALLOWLIST`, рестарт — `BlockForeignPrivateChats`
     помечает чат `X`, отменяет намерения. Корректно.
  2. id `X` вернули в список, рестарт — стартовый код ничего не разблокирует
     (условие `NOT EXISTS (… blocked_chats …)` пропускает уже помеченные).
  3. Итог: пользователь из списка, но `latestPrivateChat` его не выбирает (исключён),
     `runGates` отменяет его намерения «чат заблокировал бота», `createIntention`
     отвечает `blocked`. Лечится только следующим входящим сообщением пользователя
     (`ingest.go:149-153` делает DELETE, т.к. чат снова allowed) — тихая инициатива
     «первым» невозможна неопределённо долго, операторская правка списка выглядит
     неработающей.
- Фикс: на старте (рядом с `BlockForeignPrivateChats`) удалять пометки разрешённых:
  `DELETE FROM blocked_chats WHERE chat_id = ANY($1)` — безопаснее только с причиной
  allowlist: `... AND reason = 'Forbidden: private chat is not in allowlist'`,
  чтобы не стирать настоящие блокировки «bot was blocked». Риск гонки с настоящей
  блокировкой самоисцеляется: следующая доставка получит 403 и пометит заново.
  Тест: БД-тест «пометили чужого → добавили в allow → рестарт-функция → unblocked,
  намерение создаётся».

## MINOR

### m1. Стартовый блок видит только чаты с историей в social_events

- Файл: `internal/telegram/allowlist.go:131-134`.
- Сценарий: намерение (`intentions`) или строка outbox с `target_chat_id = чужой`,
  по которому ещё нет ни одного `social_events` (намерение создано фоном до включения
  whitelist / вручную), на старте не помечается. `runGates` проверяет `ChatBlocked`
  до модели (`executor.go:581`) — но пометки нет, гейт пропускает → **один модельный
  вызов** на генерацию check_in/share → доставка упирается в GuardAPI 403 → только тогда
  `MarkChatBlocked`. То есть «фон не тратит модель на чужую личку» нарушается ровно на
  один вызов на каждое такое осиротевшее намерение (плюс остаточные pending-turn'ы
  прямого пути: `worker`/`character.Runner` вообще не проверяют `blocked_chats` до
  модельного вызова — см. поиск по `internal/character`, `internal/worker`: гейт есть
  только в `createIntention` и в executor).
- Самозатухает после первого провала, в установившемся режиме недостижимо (новых
  намерений/turn'ов нет). Фикс (на выбор): стартовый UNION
  `social_events ∪ intentions(target, pending/running) ∪ delivery_outbox(chat, pending)`
  при сборе чужих чатов; либо проверка allowlist в месте создания turn/intention.
  Тест: сид осиротевшего намерения без истории → стартовая функция его отменяет.

### m2. Валидация TELEGRAM_PRIVATE_ALLOWLIST принимает бессмысленные id

- Файл: `internal/runtime/config.go` (`parseIDList`, из диффа).
- `allows()` (`allowlist.go:49-55`) возвращает `true` для любого `chatID < 0` независимо
  от списка, а синтетический чат `0` (неподдерживаемые updates, `normalize.go:267-273`)
  никогда не используется для отправки. Тем не менее `TELEGRAM_PRIVATE_ALLOWLIST="-5,0"`
  парсится молча и создаёт ложное ощущение настройки. Дубликаты тоже молча глотаются
  (безвредно — map).
- Фикс: ошибка старта на `id <= 0` («user id должен быть положительным») — fail-fast
  вместо молчаливого игнора. Тест — две строки в `config_allowlist_test.go`.

### m3. Несогласованность слоёв: Apply смотрит ChatType, Guard — только знак chat_id

- Файлы: `internal/telegram/allowlist.go:39-44` (проверка `ChatType != ChatPrivate`) против
  `:49-55` (проверка `chatID < 0`).
- Реальный Telegram здесь согласован: id лички положителен и равен user id, id
  групп/супергрупп/каналов отрицательны; `chat.type` из API авторитетен, Guard вторым
  слоем не навредит (положительный «групповой» id и отрицательная «личка» от настоящего
  Telegram прийти не могут; `chat_id=0` для отправки не используется).
- Но слои дают разные ответы на сконструированном входе: `chat.type=group, chat_id=+42`
  → `Apply` пропускает (pending → ход → модельный вызов), а `GuardAPI.deny(+42)` режет
  отправку. Итог — только потраченный модельный вызов, не утечка. Фикс defence-in-depth:
  либо Guard тоже учитывает тип (сигнатура `deny` без типа — пробросить ChatType),
  либо `Apply` дополнительно требует `chatID > 0` для private. Тест-строка в каждую
  сторону. Серьёзность низкая именно потому, что источник — только поддельный update.

### m4. Пробелы в тестах (покрытие есть, но не полное)

- Есть и хорошие: `allowlist_test.go` (Apply по 5 кейсам, все 6 методов Guard, 403→
  `ChatBlockedError`→`outcomeFailed`, passthrough разрешённых, nil→без обёртки),
  `allowlist_db_test.go` (ingest journal_only + неснятие блокировки + стартовый блок
  с отменой намерения), `config_allowlist_test.go` (пусто/пробелы/мусор).
- Нет, а стоило бы:
  1. правка (`edited_message`) и реакция (`message_reaction`, вкл. снятие) чужого →
     journal_only (порядок `Apply` после реакционной логики в `ingest.go:125` сейчас
     верный, но тестом не зафиксирован);
  2. дубль пачки чужого: `Inserted=false`, offset в памяти `update_id+1`
     (`poller.go:158`), повтор не создаёт ход;
  3. `poller.onPending` не вызывается для journal_only (ключевая гарантия «модель не
     вызывается» — сейчас покрыта лишь косвенно);
  4. `GetUpdates`/`GetMe` через Guard проходят (тривиально из-за embedding, но одна
     строка страхует от будущей явной реализации с deny внутри);
  5. M2/m1: разблокировка разрешённых на старте; отмена осиротевших намерений.
- БД-тесты прогнать не мог: `scripts/test-db.sh` требует эфемерный PostgreSQL 17+pgvector
  (в этой среде не поднимал; боевую базу `5433/dio_go` не трогал — требование соблюдено).
  Перед мёржем дальше — `scripts/test-db.sh` с `db`-тегом обязателен, т.к. главный
  новый тест (`allowlist_db_test.go`) именно там.

### m5. Наблюдаемость: allowlist-блоки смешаны с настоящими в blocked_chats

- Файлы: `internal/telegram/blocked.go:87-95`, метрики `dio_blocked_chats`.
- Чужой-по-whitelist чат виден в `/stats` и `dio_blocked_chats` так же, как «пользователь
  заблокировал бота». Причина (`reason = allowlistDenied`) различима в таблице, но в
  агрегатах — нет. После включения whitelist скачок метрики легко прочитать как
  «массовые блокировки пользователями».
- Фикс на выбор: отдельный счётчик/лэйбл причины либо строка в стартовом логе
  (есть: `app.go:428` — этого уже почти достаточно; добавить reason в `/stats`).

## Проверено и чисто (фиксирую, что смотрел)

1. **Выход, все 6 методов.** `API` (`api.go:136-145`) — ровно `SendMessage/SendSticker/
   SendPhoto/ForwardMessage/SendChatAction/SetMessageReaction` с chat_id; Guard
   перекрывает все 6, `GetUpdates`/`GetMe` (без chat_id) проходят через embedding.
   Dry-run-обёртка тоже заворачивается (`app.go:420` — после `buildTelegramAPI`).
2. **Ingest.** `IngestUpdateFiltered`: `Apply` после правок/реакций (`ingest.go:125`);
   journal_only пишется той же транзакцией с offset (`GREATEST`, `:126-162`) — offset
   не застревает; дубли (`ON CONFLICT DO NOTHING` + `inserted=false`) хода не создают;
   снятие блокировки подавлено только для чужих личек (`:149-153`), для своих и групп —
   как раньше. `Apply` никогда не повышает journal_only→pending.
3. **Фон.** `boredom.latestPrivateChat` (`boredom.go:225-240`) и `life_events.go:469`
   исключают blocked; `runGates` (`executor.go:581-585`) отменяет до модели;
   `createIntention` (`apply.go:759-765`) не создаёт в blocked. Групповые инициативы
   (`group_share.go`, check_in-никогда-в-группу) не перенацеливаются в лички.
4. **Recovery.** `RecoverStalePending`/`RecoverStaleSending`, устаревание доставки
   (`delivery.go:632-646`) оперируют pending/sending — journal_only туда не попадает;
   зависший sending в чужую личку при recovery уходит в unknown с отменой зависимых,
   повторной отправки нет.
5. **Аватарки/медиа.** `AvatarFetcher`/`MediaFetcher` ходят отдельными клиентами по
   токену (не через Guard), но вызываются только из хода (`enqueueAvatarCheck` после
   `Applied`, `turn.go:860`; фото/видео — сборка хода) — ходов по чужим личкам нет,
   очередь фоновых проверок пополняться нечем. Отдельный `ProfilePhoto(getUserProfilePhotos)`
   на чужого без хода не вызывается.
6. **403-конвейер.** `deny` → `APIError{403, "not in allowlist"}` → `ChatBlockedError`
   (маркер, `:91`) → `classifySendError` → `outcomeFailed` без повторов →
   `MarkChatBlocked` + отмена намерений (`delivery.go:492-500`). Постоянная ошибка —
   правильно failed, не retry.
7. **Старт.** `BlockForeignPrivateChats` до `getMe`/delivery/scheduler (гонок нет);
   запрос — по авторитетному `chat_type='private'` (`allowlist.go:133`), уже помеченные
   пропускаются; ошибка БД → `return a.stopFailed(...)` (`app.go:424-427`) — fail-closed,
   процесс не стартует полузакрытым. Пустой список — фильтр выключен везде
   (`NewPrivateAllowlist→nil`, `GuardAPI` возвращает исходный api, `Block…` early-return,
   `SetPrivateAllowlist` не вызывается).
8. **Конфиг/доки.** `parseIDList`: пробелы/висячая запятая ок, мусор — ошибка с именем
   переменной; `.env.example`/`RUNBOOK` соответствуют коду («пусто — открыто»).
   `allow` после построения только читается (конкурентно безопасно).

## Приоритет фиксов

1. M1 — перенос создания `TypingPulse` под GuardAPI (1–3 строки + тест).
2. M2 — разблокировка разрешённых id на старте по `reason = allowlistDenied` + БД-тест.
3. m1 — стартовый сбор чужих чатов из `intentions`/`delivery_outbox` (или гейт в создании
   turn), чтобы не тратить по одному модельному вызову на остаток.
4. m2, m3, m5 — валидация конфига, согласование слоёв, метрика (дешёво, по желанию).
5. m4 — дописать 5 тестовых кейсов; прогнать `scripts/test-db.sh` (эфемерный PG,
   боевую `5433` не трогать).
