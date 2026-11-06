# Повторное независимое ревью этапа 41 (2026-11-06)

Диапазон: `git diff main...bb/41-33-39-thr_ucz4hsjj6e` (коммиты ff57620, 20465ae, 182a204, e86b1bb).
Метод: чтение диффа (65 файлов, +2386/−415) + прогоны, а не только чтение.
Проверено прогоном из `git archive bb/41-33-39-thr_ucz4hsjj6e` (не из рабочей папки):
`go build ./...` — чисто; `go test ./...` — зелёный по всем пакетам;
`scripts/test-db.sh` — зелёный по всем пакетам, кроме известного `TestMainLoop` (§5).
Код не менялся (временная копия со 150-секундным бюджетом `eventually` для диагностики
TestMainLoop собиралась в `/tmp` и не коммитилась).

## Вердикт: вливать можно

Новых BLOCKER/MAJOR нет. Все B1 и M1–M10 закрыты не формально: каждый фикс покрыт
тестом-репро, проверяющим именно путь находки (прогоны ниже — все зелёные).
Отложенные m6(б), m8, m9, m15, m19, m20, m21, m27 — обоснования состоятельны (§2).
Остаточные minor-замечания — в §3 (не блокируют).

## §1. B1, M1–M10 — проверка закрытия

| # | Вердикт | Где фикс | Чем подтверждено (прогон) |
|---|---|---|---|
| B1 | закрыта | `internal/character/middleware.go:453` (`OnLLMCall` считает модельные ходы), `:498` (`modelCalls > warnedAtCall`) | `TestStopCraneDoesNotFireInsideOneToolBlock` — 4 вызова одним блоком при лимите 2 → 2 модельных запроса, `tool_choice=[false true]`, ответ текстом, не 🤔. Регресс 39.1 жив: `TestTurnToolLimitStopCraneDirectReacts`. |
| M1 | закрыта | `internal/character/web.go:461` (`verifyImage`), `:481` (`fetchImage`), `internal/character/turn.go:286`, `internal/character/journal.go:143` (`WriteModelOutputWithImage`) | БД `TestRecoveryDoesNotFetchUnverifiedImageDB` (сырой `image_url` без вердикта не качается, фото в outbox нет), `TestRecoveryUsesSavedImageVerdictDB` (вердикт качается, alt «кот» на месте); юниты `TestVerifyImageRejectsURLOutsideWebOffer`, `TestRecoveryImageUsesSavedVerdict`. Порядок «проверка → запись → скачивание» — по коду. |
| M2 | закрыта | `internal/character/web.go` (`webURLCollector.addStructured`, `collectPageImageURLs`, `collectSearchResultURLs`, `webResultMW` только по `DataInput`), `internal/web/tools.go` (web_fetch отдаёт `DataInput{images}` + `ImagesBlockHeader`) | `TestWebProvenanceOnlyStructured` (поддельный заголовок в тексте, страница без картинок → отказ), `TestSearchProvenanceReadsRealStructures` (настоящие `toolpacks.*SearchResponse`). Проверено по коду ago v0.6.0: struct-возврат FunctionTool идёт через `EnsureInput` → `DataInput`, `normalizeJSONData` его разбирает — ветка поиска живая, а не мёртвая. |
| M3 | закрыта | `internal/character/turn_result_normalize.go:273` (`canDegradeEmptyAnswer`), `internal/character/turn.go:687` | `TestCanDegradeEmptyAnswer`, БД `TestRunDirectEmptyAnswerIsSkippedDB` (прямой ход с пустым answer после repair — терминальный пропуск, не `ignore`). Непрямой ход — прежняя деградация 38.2. |
| M4 | закрыта | `internal/character/middleware.go` (`SetClosed` больше не ставит `closedByReject`; новый `SetClosedByLimit` для отказа суточным веб-лимитом) | `TestClosedByRejectOnlyForLimit` + регресс `TestTurnToolLimitStopCraneDirectReacts`. |
| M5 | закрыта с остатком | `internal/character/model.go:206` (`isContextLengthError` исключён из permanent: 8 маркеров длины) | `TestPermanentProviderErrorExcludesContextLength`. Остаток (заявлен исполнителем): урезания эпизода нет — 400 по длине теперь жжёт до 5 попыток повтором того же контекста вместо cooldown 24 ч. Лучше, чем было; отдельный minor-регресс учёта — нет. |
| M6 | закрыта | `internal/character/initiative_limit.go:42` (`IsDailyLimitedInitiative` = только check_in/share), `apply.go` (defer создаёт reply_later всегда), `background/executor.go` + `group_share.go` (гейты: reply_later — только пауза `proactiveGapAllowed`), `panel.go:185` (панель говорит про defer) | `TestDeferNotDailyLimited`, `TestInitiativeDeferralUntil`, БД `TestApplyDefersReplyLaterAtDailyLimitDB`, обновлённый `TestProactiveCaps`. `ProactiveUsage` идёт через тот же `ReadInitiativeUsage` (reply_later не в дневном счёте, но в `Last` для паузы) — учёт целостен. |
| M7 | закрыта | `internal/character/image_notes.go:74` (нумерация позиций 1..N), `storyboard.go` (поле `First` удалено), `turn_result_schema.go:87` (текст «на позицию, всего N») | `TestImageNumberingCountsPositions`, `TestImageNumberingLine` (фото + гифка 5 кадров + фото → 3 позиции, фразы не съезжают). |
| M8 | закрыта | `internal/character/text_sanitize.go:109` (`sanitizeInlineNote`: `\n\r`, контролы, `[ ]`, `#m`-метки), `turn_result.go` (валидатор — второй пояс: `\n` в note → ошибка) | `TestImageNoteRejectsNewline`. Двусторонняя защита: нормализация чистит, валидатор ловит остатки. |
| M9 | закрыта | `internal/character/turn_result_normalize.go:45` (точные фразы + короткое уточнение ≤20 рун без `—–:;,`) | `TestJunkOptionalStringKeepsMeaningfulPrefix` («не нужно откладывать — дата поджимает», «Нечего бояться» сохраняются). Остаточный край — очень короткая причина без пунктуации («не нужен срочно») — в §3, R1. |
| M10 | закрыта | `internal/background/boredom.go:342` (`≥2 слов или ≥3 букв`), однословные вопросы («как», «что», …) — в мусоре | `TestValidCuriosityThemeShortWord`, обновлённый `TestValidCuriosityTheme`. «Море»/«лес»/«джаз» теперь темы. |

## §2. Отложенные m* — оценка обоснований

Все 7 (8-е, m6, — частично: (а) сделано, (б) отложено) — убедительно:

- m6(б) кооперативная отмена running-намерения: нужен протокол «отменено между шагами» и семантика перезаписи статуса исполнителем — отдельная работа, не однострочник. Running-задачи короткие. Отложено обоснованно.
- m8 (429-регекс/учёт 3 wire-запросов): ужесточение без живых образцов рискует потерять легитимный повтор; митеринг «3 запроса на 1 резерв» — продуктовое решение. Паузы ограничены N≤10, отмена ctx — корректна. Не приватность/доставка. Отложено обоснованно (остаток — в §3, R2).
- m9 (гонка дневного лимита поисков): последовательный worker смягчает; условный инкремент меняет семантику счётчика. Отложено обоснованно.
- m15 (идемпотентность счётчика по `(turnID, tool_call)`): редкий сценарий (транзитный провал после поисков), ущерб ограничен дневным лимитом. Отложено обоснованно.
- m19 (хардкод 15 в метрике): настраиваемого лимита нет — прокидывать опцию значит плодить мёртвую конфигурацию. В коде оставлен маркер. Отложено обоснованно.
- m20 (escape только если `json.Valid`): не чинит разбалансированные кавычки; надёжный фикс — толерантный парсер (отдельная работа). Ход не теряется молча (repair просит JSON заново). Отложено обоснованно.
- m21 (`ByEmoji` full scan): коллекция персоны — десятки строк; нормализованная колонка + миграция ради микросекунд. Отложено обоснованно.
- m27 (RAM-бюджет видео): потолки 20 МиБ/видео и 6 картинок ограничивают худший случай (~40+ МБ transient при двух видео). Отложено обоснованно (остаток — в §3, R3).

Исправленные minor перепроверены прогоном, каждый — по своему пути: m1 (`TestDegradeReactClearsForward`),
m2 (`TestUnknownToolFallsUnderOtherCap`), m3 (`TestSchemaRejectionSecondFallback` — 3 wire-запроса),
m4 (`TestUsageCounter*`, pendingCap со следующего хода), m5 (БД `TestIntentionPermanentProviderErrorFailsOnceDB`),
m6(a) (только 403; `TestBlocked*`), m7 (guard-строка в constraints), m10 (`TestStripMessageLabelsDoesNotDamageUserText` —
URL/`M100`/`#2024` не портятся), m11 (`TestOneLineAlt`, ≤300 рун), m12 (мьютекс), m13 (БД `TestDeliveryPhotoFailsTerminallyDeletesBytes`),
m14 (`TestRedactURLSecrets`; `model_output` хранит проверенный URL с query — осознанный компромисс: нужен восстановлению),
m16 (`TestWebUsageCountsOnlySuccessfulSearches`), m17 (БД `TestDeliveryPhotoMarksGroupInvolvement`),
m18 (alt персистится, санитизируется на чтении), m22 (лог несовпавшей метки), m23 (advisory-лок чата в транзакции применения,
БД `TestConcurrentApplyKeepsDailyLimitDB`), m24 (`%q` + пометка), m25 (сессия `:avatar`), m26 (маркер `invisible_reported`,
миграция 000025, БД `TestAvatarInvisibleAfterWritesOnceDB`), m28 (снятие реакции — всегда `journal_only`),
m29 (время слота — часами БД), m30 (`ErrAnswerWithoutText` через `%w`, `errors.Is` в repair/деградации — цепочка не рвётся:
`parseTurnResult` возвращает ошибку `Validate` как есть).

## §3. Новые регрессии от исправлений — не найдены; остатки minor

- Полный `scripts/test-db.sh` по архиву ветки: character, background, telegram, memory, metrics, runtime, worker, web, media, avatar, app (кроме TestMainLoop) — всё зелёное. Новых падений относительно main нет.
- `kindCapOver` «прочие» (m2) действует и на фоновый `ToolLimitMiddleware` (`background/modelrun.go:209`) — это ужесточение по дизайну 38.4, известные наборы (память/веб) не тронуты.
- `webUsageMW` (m16): резерв при входе + возврат при неуспехе; DB-счётчик только за успех; рестарт пересчитывает остаток из БД. Расхождений нет.
- Advisory-лок `apply-chat:<id>` (m23): xact-scope, разные чаты — разные ключи, дедлока нет; конкурентный тест зелёный.
- R1 (minor, остаток M9): «не нужен срочно» (коротко, без пунктуации) всё ещё дропается. Редко и только в reason/topic; задокументировано исполнителем.
- R2 (minor, остаток m8): свободный регекс `after N s` и квотный 429 — поведение до этапа 41, не ухудшено.
- R3 (minor, остаток m27): два видео ≈ 40+ МБ transient. Потолки держат; бюджет — отдельной работой.
- R4 (minor, остаток M5): 400 по длине контекста теперь сжигает до 5 попыток вместо cooldown. Лучше cooldown-24ч; урезание эпизода — будущая работа.
- R5 (minor, принято исполнителем): короткие терпимые метки `#12`/`m12` не вырезаются (защита живого текста); каноническая `#m<любые цифры>` режется везде, включая `image_notes`/память/намерения.

## §4. Сборка из git, а не из рабочей папки

`git archive bb/41-33-39-thr_ucz4hsjj6e | tar -x -C <tmp>`: `go build ./...` — чисто (exit 0),
`go test ./...` — зелёный по всем 13 пакетам. Этап 40 не собирался из-за `.gitignore`
(не включён в архив) — здесь проверено: архив ветки собирается и тестируется из чистого дерева.

## §5. TestMainLoop — подтверждено: ограничение окружения, не регрессия ветки

- Архив ветки: 2 прогона `scripts/test-db.sh ./internal/app/ -run TestMainLoop` — оба FAIL, одинаково:
  `claim записан с источником turn:1`, затем `условие не выполнено за 30s: stance эпизода 1 применён
  и инициатива доставлена` (`e2e_main_loop_db_test.go:196`).
- Архив main: 2 прогона — тот же FAIL, та же строка, то же время (~31.5 с).
- Диагностика: копия архива ветки с бюджетом `eventually` 150 с — всё равно FAIL (2м30с).
  Причина — не скорость, а логика: тихий блок расписания в `config/persona.md` откладывает
  инициативу (бисект родителя: падает одинаково на a3dd416, c5041cd, 5e35146, 03dd0b4, зависит
  только от времени запуска). В бэклог, не блокирует этап 41.
- Остальной `internal/app` на ветке (`-skip TestMainLoop`) — зелёный.

## Находки по severity

- BLOCKER: нет.
- MAJOR: нет.
- MINOR (остатки, не блокируют): R1 (M9-край), R2 (m8-регекс), R3 (m27-RAM), R4 (M5-повторы), R5 (короткие `#12` — принято).
- Отложенные m6(б)/m8/m9/m15/m19/m20/m21/m27 — обоснованы, приняты.
