/*
zcode-workflow:
  name: Переезд dio-agent на Go (этап 9)
  description: Полный перенос проекта с TypeScript на Go с событийным ядром ago по спеке v0.7 — модули 9.1–9.8, гейты после каждого модуля, приёмка ревьюером, коммит
  arguments:
    - name: startCommit
      type: string
      description: Коммит-точка старта для финального независимого ревью (дифф переезда)
      required: false
      default: "7b476cc"
*/

// ── Константы и общие тексты ──────────────────────────────────────────────

const START_COMMIT = String(args.startCommit || "7b476cc");

const CONV = `
Общие правила этапа 9 (переезд dio-agent на Go):
- Источники: docs/ARCHITECTURE_v0.7.md — спецификация, источник истины; docs/GLOSSARY.md — термины; docs/PHILOSOPHY.md — приоритеты; твой пункт в CHECKLIST.md. TypeScript-код в src/ — справочник поведения, не образец архитектуры: переноси поведение и гарантии, а не структуру.
- Фреймворк: third_party/ago (module path "ago", подключается replace-директивой; документация third_party/ago/docs/ — начни с GUIDE.md и ARCHITECTURE.md). Сам фреймворк не редактируй: если нашёл его дефект — обойди или внеси минимальный патч, отметь в agoPatches и проверь "go test ./..." внутри third_party/ago.
- Зависимости приложения: pgx/v5 (Postgres) и pgvector-go при необходимости; больше ничего без веской причины (философия №6 — минимализм).
- Гейты (их прогонит воркфлоу после тебя, полный suite сам не гоняй — точечные go test по своим пакетам запускай): gofmt -l . даёт пустой вывод; go build ./...; go vet ./...; go test ./... — юниты без живой БД; go test -tags db ./... — против Postgres docker-compose, он уже запущен, DSN по умолчанию postgres://dio:dio@localhost:5432/dio?sslmode=disable, переменная TEST_DATABASE_URL переопределяет. БД-тесты сами применяют свою схему к отдельной тестовой базе (создать/залить/удалить) и делают t.Skip с понятной причиной, если БД недоступна.
- Go-файлы — LF (в 9.1 добавляется .gitattributes). Файлы маленькие, одна ответственность; идентификаторы — по-английски; комментарии, логи, сообщения коммитов — по-русски.
- git commit сам не делай — коммитит воркфлоу. Галочку [x] своего пункта в CHECKLIST.md поставь, когда пункт закрыт.
- Если гейт невозможно пройти или инструкции противоречат спеке — прямо скажи об этом в ответе, не имитируй готовность.`;

const IMPL_SYSTEM =
  "Ты — опытный Go-разработчик, выполняешь один модуль переезда dio-agent с TypeScript на Go по спецификации v0.7. " +
  "Работаешь в корне репозитория dio-agent. Решения — по спеке и философии проекта; при противоречии — спека. " +
  "Если гейт невозможно пройти или задача противоречит спеке — подними вопрос и скажи об этом прямо, вместо обхода.";

const REVIEW_SYSTEM =
  "Ты — строгий независимый приёмщик кода. Ты не редактируешь файлы. " +
  "Ищи, что сломается и чего не хватает против спецификации и пункта чек-листа: читай код, спеку, сверяй поведение с TS-справочником. " +
  "Замечания — только подтверждённые факты с путями и строками; не выдумывай придирок, но и не пропускай нарушений спеки и философии. " +
  "Сомневаешься в замечании — проверь его кодом или точечным тестом.";

interface ImplResult {
  /** Краткое резюме принятых решений (1–3 предложения) — пойдёт в сообщение коммита. */
  summary: string;
  /** Дефекты ago и что с ними сделали; пусто, если не было. */
  agoPatches: string;
  /** Что сознательно отложено и почему; пусто, если ничего. */
  deferred: string;
}

interface Verdict {
  /** "pass" — модуль принят; "fix" — есть обязательные замечания. */
  verdict: "pass" | "fix";
  /** Обязательные к исправлению замечания; пусто при pass. */
  mustFix: string[];
  /** Неблокирующие пожелания. */
  notes: string[];
}

interface GateResult {
  ok: boolean;
  /** Сводка упавших проверок: имя + хвост вывода. */
  failures: string;
}

// ── Детерминированные гейты ───────────────────────────────────────────────

async function runGates(): Promise<GateResult> {
  const failures: string[] = [];
  const fmt = await world.run("gofmt", ["-l", "."]);
  if (fmt.stdout.trim() !== "") {
    failures.push("=== gofmt -l . (файлы не отформатированы) ===\n" + fmt.stdout.slice(0, 1500));
  }
  const build = await world.run("go", ["build", "./..."], { timeoutMs: 300000 });
  if (build.exitCode !== 0) failures.push("=== go build ./... ===\n" + (build.stderr || build.stdout).slice(-2500));
  const vet = await world.run("go", ["vet", "./..."], { timeoutMs: 300000 });
  if (vet.exitCode !== 0) failures.push("=== go vet ./... ===\n" + (vet.stderr || vet.stdout).slice(-2500));
  const unit = await world.run("go", ["test", "./..."], { timeoutMs: 600000 });
  if (unit.exitCode !== 0) failures.push("=== go test ./... ===\n" + (unit.stderr || unit.stdout).slice(-2500));
  const db = await world.run("go", ["test", "-tags", "db", "./..."], { timeoutMs: 900000 });
  if (db.exitCode !== 0) failures.push("=== go test -tags db ./... ===\n" + (db.stderr || db.stdout).slice(-2500));
  return { ok: failures.length === 0, failures: failures.join("\n\n") };
}

async function commitStage(message: string): Promise<string> {
  await world.run("git", ["add", "-A"]);
  const c = await world.run("git", ["commit", "-m", message], { timeoutMs: 300000 });
  if (c.exitCode !== 0) {
    throw new Error("git commit не прошёл:\n" + (c.stderr || c.stdout).slice(-2000));
  }
  const h = await world.run("git", ["rev-parse", "--short", "HEAD"]);
  return h.stdout.trim();
}

// ── Шаблон стадии модуля (фаза объявляется в месте вызова) ───────────────

async function migrateModule(m: {
  num: string;
  name: string;
  ask: string;
  reviewAsk: string;
}): Promise<void> {
  log(`${m.num} ${m.name}: старт`);
  const impl = agent(`исполнитель-${m.num}`, { system: IMPL_SYSTEM });
  const done = await impl.ask<ImplResult>(m.ask + CONV);
  report({ module: m.num, name: m.name, status: "в работе", notes: done.summary }, "modules");

  // Гонка «сделал → гейты зелёные», до 4 раундов правок
  let gates = await runGates();
  let round = 1;
  while (!gates.ok && round <= 4) {
    log(`${m.num}: гейты красные, раунд правок ${round}`);
    await impl.ask<string>(
      `Гейты не зелёные:\n${gates.failures}\n\nИсправь. Полный suite после тебя снова прогонит воркфлоу.`,
    );
    gates = await runGates();
    round++;
  }
  if (!gates.ok) {
    throw new Error(`${m.num}: гейты не зелёные после 4 раундов правок:\n${gates.failures.slice(0, 3000)}`);
  }
  report({ module: m.num, name: m.name, status: "гейты зелёные", notes: done.summary }, "modules");

  // Приёмка независимым ревьюером, до 3 раундов исправлений
  const rev = agent(`приёмщик-${m.num}`, { system: REVIEW_SYSTEM });
  let verdict = await rev.ask<Verdict>(m.reviewAsk + ` Гейты уже зелёные — их прогнал воркфлоу, полный suite сам не гоняй.`);
  let fixRound = 1;
  while (verdict.verdict === "fix" && fixRound <= 3) {
    log(`${m.num}: приёмщик вернул замечания, раунд ${fixRound}`);
    await impl.ask<string>(
      `Приёмщик требует исправить:\n${JSON.stringify(verdict.mustFix)}\n\nИсправь только это. Гейты после правок прогонит воркфлоу.`,
    );
    gates = await runGates();
    if (!gates.ok) {
      throw new Error(`${m.num}: гейты упали после правок по замечаниям:\n${gates.failures.slice(0, 3000)}`);
    }
    verdict = await rev.ask<Verdict>(
      `Правки внесены. Пере-проверь только свои обязательные замечания: ${JSON.stringify(verdict.mustFix)}`,
    );
    fixRound++;
  }
  if (verdict.verdict !== "pass") {
    throw new Error(`${m.num}: приёмщик не принял после 3 раундов: ${JSON.stringify(verdict.mustFix)}`);
  }
  report({ module: m.num, name: m.name, status: "принят", notes: verdict.notes.join("; ") || done.summary }, "modules");

  const notes = [done.summary, done.agoPatches ? `ago: ${done.agoPatches}` : "", done.deferred ? `отложено: ${done.deferred}` : ""]
    .filter(Boolean)
    .join(" | ");
  const sha = await commitStage(`Этап ${m.num} ${m.name}: ${done.summary}`);
  report({ module: m.num, name: m.name, status: "закоммичен", notes }, "modules");
  log(`${m.num} закоммичен: ${sha}`);
}

// ── Доска прогресса ───────────────────────────────────────────────────────

artifact.board("modules", {
  title: "Прогресс переезда на Go",
  key: "module",
  status: "status",
  columns: ["в работе", "гейты зелёные", "принят", "закоммичен"],
  cardTitle: "name",
  detail: [{ field: "notes" }],
});

// ── 9.1 Каркас Go ─────────────────────────────────────────────────────────

phase("9.1 Каркас Go (go.mod, структура, миграции, хук-гейт)");
await migrateModule({
  num: "9.1",
  name: "Каркас Go",
  ask: `Модуль 9.1 «Каркас Go» (CHECKLIST 9.1). Сделай:
- корневой go.mod: module github.com/luckystrker/dio-agent, require github.com/jackc/pgx/v5, replace ago => ./third_party/ago; .gitattributes с eol=lf для *.go;
- структуру cmd/agent и internal/{telegram,character,memory,background,runtime} с минимальными пакетами;
- загрузку конфигурации из переменных окружения (сверь список с .env.example и src/config.ts) с валидацией при старте: неизвестные/невалидные значения — явная ошибка запуска;
- миграции migrations/*.sql + маленький собственный раннер (таблица schema_migrations, применение по порядку, идемпотентно) и ПОЛНУЮ начальную схему из спеки §18 — все таблицы, включая event_streams (UNIQUE(stream_id, seq), append-only) и task_checkpoints, индексы, extension vector;
- точку входа cmd/agent/main.go: конфиг → подключение БД (pgx) → миграции → сигнал graceful shutdown;
- перепиши хук-гейт .zcode/hooks/pre-commit-gate.mjs на Go-гейты (gofmt -l ., go vet, go test — без живой БД; package.json уже не источник гейтов).
БД-тесты раннера миграций (применение на чистой базе дважды — без ошибок). Резюме решений — в summary.`,
  reviewAsk: `Прими модуль 9.1 «Каркас Go». Прочитай CHECKLIST 9.1, спеку §18 (схема БД), §5.3 (startup/shutdown), §17 (вендорная копия); код: go.mod, cmd/, internal/, migrations/, хук. Сверь состав таблиц с §18 — таблица не должна потеряться или исказиться, event_streams — с UNIQUE(stream_id, seq). Что сломается? Чего не хватает?`,
});

// ── 9.2 Адаптеры ago↔PostgreSQL ──────────────────────────────────────────

phase("9.2 Адаптеры хранилища (журнал, чекпоинты, WAL)");
await migrateModule({
  num: "9.2",
  name: "Адаптеры хранилища",
  ask: `Модуль 9.2 «Адаптеры ago↔PostgreSQL» (CHECKLIST 9.2). Спека §12, §18; TS-справочник восстановления — src/runtime. Сделай поверх ago:
- PgStreamStorage — реализацию интерфейса Storage из ago/stream (изучи third_party/ago/stream и third_party/ago/docs/ARCHITECTURE.md: SaveEvent/GetHistory/SetHistory/DropHistory) на таблице event_streams: запись с UNIQUE(stream_id, seq), семантика однократной записи — сверяй с исходниками ago, не изобретай;
- PgCheckpointStore — реализацию CheckpointStore из ago/task (Write/Read) на task_checkpoints;
- WAL фоновых задач — запись намерений шагов в event_streams отдельными потоками (спека §12);
- БД-тесты (тег db): однократность (повтор той же пары stream+seq не создаёт второе событие), replay — записанная последовательность читается бит-в-бит, чекпоинт round-trip, обрыв последовательности виден при чтении.`,
  reviewAsk: `Прими модуль 9.2 «Адаптеры хранилища». Прочитай CHECKLIST 9.2, спеку §12 (событийный журнал, таблица восстановления) и §18; код адаптеров и их БД-тесты. Ключевое: семантика интерфейсов совпадает с ago (сверь с исходниками third_party/ago), однократность записи гарантируется схемой, replay восстанавливает состояние. Что сломается при сбое посреди записи?`,
});

// ── 9.3 Память ────────────────────────────────────────────────────────────

phase("9.3 Память (репозитории, поиск, инструменты модели)");
await migrateModule({
  num: "9.3",
  name: "Память",
  ask: `Модуль 9.3 «Память» (CHECKLIST 9.3). Спека §8, §9, §10, §18; TS-справочник — src/memory. Сделай:
- репозитории entities, entity_aliases, memory_items, memory_links, sources, memory_sources, memory_derivations, provenance, stances, stance_basis поверх миграций 9.1;
- поиск: exact/alias, full-text (tsvector), семантический pgvector (эмбеддинги через OpenAI-совместимый endpoint из конфигурации; если ключ не настроен — контракт + фейковый векторизатор для тестов);
- retrieval как ago-инструменты только для чтения (объяви через ago/tools со схемами аргументов): поиск сущностей, карточка сущности (stances + memories + основания), текстовый поиск;
- приватность-фильтр §9.2 ДО выдачи результата модели: scopes public / chat:<id> / internal, пересечение разрешений производных объектов, missing/invalid provenance → internal;
- флаг forced tool-calling на инструменте (проводку на прямом обращении сделает модуль 9.4);
- БД-тесты: scope-изоляция (приватное из лички не всплывает в группе — через инструмент, не только через репозиторий), supersede-цепочки, фильтр срабатывает до выдачи.`,
  reviewAsk: `Прими модуль 9.3 «Память». Прочитай CHECKLIST 9.3, спеку §8 (виды записей), §9 (приватность: §9.2 где проверяется доступ), §10.4 (retrieval как инструмент); код и тесты. Ключевое: фильтр стоит ДО выдачи модели (не «выдали и надеемся»), инструменты только для чтения, модель не может изменить память. Что утечёт из лички в группу через твой код?`,
});

// ── 9.4 Персонаж и ход ────────────────────────────────────────────────────

phase("9.4 Персонаж и ход (persona, контекст, агент ago, дельты)");
await migrateModule({
  num: "9.4",
  name: "Персонаж и ход",
  ask: `Модуль 9.4 «Персонаж и ход» (CHECKLIST 9.4). Спека §7, §11, §12, §13.3, §16, §17; TS-справочник — src/character и src/runtime. Сделай:
- парсер/рендер persona-файлов: config/persona.md и config/system-prompt.md, метки {{persona}}/{{mood}}/{{relationship}}/{{memory}}, пустая метка исчезает, неизвестная метка валит запуск, файл — источник истины (отличие от активной записи создаёт новую версию в БД). Поведение TS перенеси точно (строгий парсер секций);
- mood: три параметра, затухание к baseline по формуле §7.2, ограниченные дельты с основаниями; relationships: familiarity/affinity/trust, ограниченные изменения со ссылками;
- сборку базового контекста §11.1: persona+constraints структурной секцией, batch/цель фона, mood, отношения, недавний разговор (обе стороны, до MODEL_HISTORY_MESSAGES=1000, потолок MODEL_HISTORY_MAX_CHARS, первыми теряются старые, только доставленные ответы);
- ход = запуск ago-агента (ago/agent): системный промпт из шаблона, базовый контекст, инструменты памяти 9.3; на прямое обращение — forced первый вызов инструмента; лимит обращений к инструментам конфигурируемый;
- строгая валидация TurnResult §11.2 (дельты, а не абсолюты; лимиты длин; правила action) — невалидный не применяется, retry по политике;
- события хода в event_streams: turn_started, tool_call, tool_result, model_output, deltas_applied, delivery_queued, turn_skipped; валидированный результат пишется ДО применения дельт; применение дельт + delivery_queued — одна транзакция; повторный прогон не дублирует;
- middleware ago: бюджет §16 (budget_reservations/budget_counters, дневной лимит, резерв до вызова) и typing-пульс §13.3 (старт сразу, обновление ~4 с, stop в finally);
- таблица delivery_outbox создаётся событием delivery_queued; саму отправку делает модуль 9.5.
БД-тесты: невалидный TurnResult не применяется, повторное применение не дублирует, события однократны, сбой между model_output и дельтами восстанавливается без новой генерации.`,
  reviewAsk: `Прими модуль 9.4 «Персонаж и ход». Прочитай CHECKLIST 9.4, спеку §11 (контекст и контракт результата), §12 (журнал и однократность), §17 (владение контекстом: всё, что видит модель, собирает приложение); код и тесты; сверяй с TS (src/character, src/runtime). Ключевое: модель предлагает — runtime решает (философия №7); невалидный результат не применяется; прямой вопрос гарантированно получает память (forced-вызов). Что сломается?`,
});

// ── 9.5 Telegram ──────────────────────────────────────────────────────────

phase("9.5 Telegram (клиент, ingest, буферы, outbox, доставка)");
await migrateModule({
  num: "9.5",
  name: "Telegram",
  ask: `Модуль 9.5 «Telegram» (CHECKLIST 9.5). Спека §6, §13; TS-справочник — src/telegram. Сделай тонкий клиент Bot API на net/http (без сторонних библиотек):
- getUpdates long polling, sendMessage, sendChatAction, setMessageReaction; timeout на каждый вызов, 429 — honor retry_after; клиент тестируй через httptest;
- ingest §6.1: короткая транзакция на update, UNIQUE(telegram_update_id) — конфликт = «уже сохранено», offset двигается только после записи и в той же транзакции, unsupported updates — journal_only/skipped чтобы offset не застревал, при ошибке БД offset не двигается;
- буферы §6.2: idle/max окна по типам чатов (личка/группа/reply-mention), закрытие по 20 событиям/лимиту текста, таймер берёт ближайший срок, поток не переносит закрытие бесконечно; закрытие буфера транзакционно создаёт turn и привязывает события;
- prefilter §6.3: прямой запрос не отбрасывается, шум группы — journal_only;
- outbox и delivery loop §13.1–13.3: статусы pending/sending/sent/retry_wait/unknown/cancelled/failed, порядок внутри чата, unknown-политика §13.2 (таймаут/падение после возможной отправки → unknown, автоматически не повторяется, зависимые отменяются), устаревание §13.3 (проверка новых событий перед отправкой, отмена оставшихся bubbles), события доставки — в event_streams;
- события хода 9.4 превращаются в outbox-строки и отправляются этим модулем.
Юнит-тесты: дедупликация update, окна буферов, unknown-политика; БД-тесты: восстановление outbox после сбоя, отсутствие дублей.`,
  reviewAsk: `Прими модуль 9.5 «Telegram». Прочитай CHECKLIST 9.5, спеку §6 (ingest, буферы, prefilter), §13 (честные гарантии: outbox, unknown, подача); код и тесты; сверяй с src/telegram. Ключевое: ingest никогда не ждёт модель; offset только после записи; дубли доставки невозможны по построению; unknown не повторяется вслепую. Что сломается при падении процесса между отправкой и записью?`,
});

// ── 9.6 Фон ───────────────────────────────────────────────────────────────

phase("9.6 Фон (планировщик, намерения, консолидация, наблюдаемость)");
await migrateModule({
  num: "9.6",
  name: "Фон",
  ask: `Модуль 9.6 «Фон» (CHECKLIST 9.6). Спека §10.2, §14, §15 (MVP без web), §20; TS-справочник — src/background. Сделай:
- планировщик due-работы: таймер 15–30 с с jitter, проверка не вызывает модель, прямое обращение приоритетнее — фоновый шаг переносится/отменяется (§5.2);
- intentions §14.1: kinds reply_later/check_in/share/explore, dedupe_key, notBefore/expiresAt обязательны, статусы, проверка актуальности перед запуском (тема закрыта/ответ уже дан/дубль), proactive caps §14.3 — не более 2 инициативных последовательностей на личный чат в сутки, не менее 4 часов между, в группы выключены;
- консолидация §10.2: границы эпизодов (4 ч тишины в DM, 1 ч в группе, 100 событий), episode summaries, актуализация stances — модель только предлагает, применяет runtime; повторное чтение источника не повышает confidence (§10.3);
- exploration MVP §15: модельный вызов по intention explore с ограниченным бюджетом и только инструментами памяти (без web);
- наблюдаемость §20: журнал хода одной записью (IDs, decision, outcome, latency, usage, бюджет, основания mood/отношений), queueStats + предупреждение на рост очереди, heartbeat на уровне debug;
- единица фоновой работы = ago-задача с чекпоинтом (task_checkpoints) и WAL в event_streams; возобновление с чекпоинта после сбоя.
БД-тесты: dedupe намерений, caps, возобновление задачи с чекпоинта, перенос фона при прямом обращении.`,
  reviewAsk: `Прими модуль 9.6 «Фон». Прочитай CHECKLIST 9.6, спеку §14 (намерения, планировщик, proactive policy), §20 (наблюдаемость); код и тесты; сверяй с src/background. Ключевое: просто скука не причина писать людям; создание намерения не гарантирует исполнение; фон не мешает прямым обращениям. Что сломается?`,
});

// ── 9.7 E2E и устойчивость ───────────────────────────────────────────────

phase("9.7 E2E и устойчивость (сценарии спеки на скриптованной модели)");
await migrateModule({
  num: "9.7",
  name: "E2E и устойчивость",
  ask: `Модуль 9.7 «E2E и устойчивость» (CHECKLIST 9.7). Спека §21.1, §5.1, §5.3, §1; TS-справочник — e2e-тесты src. Сделай E2E на скриптованной офлайн-модели (ago/testing, примеры в third_party/ago/docs/GUIDE.md), всё с тегом db:
- главный цикл §1 целиком: тема в разговоре → запись с источником → exploration → возврат к теме через дни (двигай время) → пересмотр позиции при новом опыте;
- сценарии устойчивости §21.1: сбой после model_output → результат применяется без новой генерации; «SIGKILL» — обрыв соединения и перечитывание журнала восстанавливает состояние; повторный запуск применённого хода не дублирует дельты; unknown-доставка не повторяется вслепую; потеря singleton-соединения прекращает новые действия;
- singleton §5.1: advisory lock, второй процесс выходит с ошибкой;
- graceful shutdown §5.3: остановил приёма → дождался текущего → закрыл соединения; recovery работает и без graceful;
- поведенческие проверки механик по docs/BEHAVIOR_SCENARIOS.md на скриптованных диалогах: молчание вместо автоматического ответа, реакция без текста, вежливость с незнакомцем, ответ на прямой вопрос при подавленной инициативе.
Если сценарий не сходится со спекой — исправляй модули, а не тесты. Это самая сильная проверка переезда.`,
  reviewAsk: `Прими модуль 9.7 «E2E и устойчивость». Прочитай CHECKLIST 9.7, спеку §21 (проверочные сценарии — сверяй таблицу 21.1 построчно с тестами), §3 (инварианты); код тестов и правки модулей, если они были. Ключевое: сценарии проверяют реальную механику (журнал/replay), а не заглушки; E2E проходит чисто. Какой сценарий §21.1 не покрыт и почему?`,
});

// ── 9.8 Завершение ────────────────────────────────────────────────────────

phase("9.8 Завершение (удаление TS, RUNBOOK, README, бэклог)");
await migrateModule({
  num: "9.8",
  name: "Завершение",
  ask: `Модуль 9.8 «Завершение» (CHECKLIST 9.8). Сделай:
- удали TypeScript: src/, package.json, package-lock.json, tsconfig*, eslint*, vitest-конфиг, а также npm-упоминания из активных конфигов; .gitignore приведи к Go-проекту (без node_modules/dist или с ними — если ещё нужны);
- README.md: быстрый старт Go (docker compose up -d, миграции, go run ./cmd/agent), команды гейтов;
- docs/RUNBOOK.md: деплой Go-бинарника на VPS (сборка Linux, systemd unit, переменные окружения, миграции из migrations/, восстановление после сбоя — по спеке §20);
- CHECKLIST.md: пункты бэклога про механики TS (work_items, SkipWorkError, in-process retry буфера, детерминированные operation_id и т.п.) пометь «(устарело при переезде на Go, 2026-10-30)» — не удаляй;
- AGENTS.md: проверь актуальность строк про структуру и гейты;
- прогони все гейты; убедись, что в активных путях не осталось ссылок на npm/node/tsc.`,
  reviewAsk: `Прими модуль 9.8 «Завершение». Проверь: TS удалён полностью и нигде в активных путях не остался; README и RUNBOOK соответствуют реальности (команды существуют, unit юнит указан верно); бэклог помечен, а не удалён; гейты зелёные. Что забыл?`,
});

// ── Финальная независимая проверка ────────────────────────────────────────

phase("Независимая проверка всего переезда");
const coldReviewer = agent("независимый ревьюер", {
  system:
    "Ты — независимый архитектурный ревьюер. Ты не редактируешь файлы. " +
    "Проверяешь весь переезд dio-agent с TypeScript на Go от точки старта: соответствие спеке v0.7, философии и инвариантам §3. " +
    "Замечания — только подтверждённые кодом факты (путь:строка). mustFix — только нарушения спеки/философии, не стилистика. " +
    "Если замечание не удалось подтвердить — не включай его.",
});
const review = await coldReviewer.ask<Verdict>(
  `Проверь весь переезд на Go. Точка старта — коммит ${START_COMMIT}: прочитай git log ${START_COMMIT}..HEAD и git diff ${START_COMMIT} по ключевым файлам. ` +
    `Затем docs/ARCHITECTURE_v0.7.md целиком и CHECKLIST этап 9. Выборочно по коду проверь инварианты §3: ` +
    `однократность применения хода (§12: журнал, UNIQUE(stream_id, seq), одна транзакция на дельты); приватность до выдачи модели (§9.2); ` +
    `модель не пишет в базу и не отправляет сама (философия №7); база — источник истины (перезапуск теряет только кэш); ` +
    `прямой вопрос получает память (forced tool-calling); outbox-гарантии §13.2. ` +
    `Гейты уже зелёные — полный suite не гоняй, точечные проверки можно.`,
);

let finalFixes = "";
if (review.verdict === "fix") {
  phase("Исправления по итогам независимой проверки");
  for (const f of review.mustFix) {
    report({ module: "финал", name: f.slice(0, 100), status: "найдено ревью", notes: "" });
  }
  const fixer = agent("исправитель финала", { system: IMPL_SYSTEM });
  const fixRes = await fixer.ask<string>(
    `Независимая проверка переезда нашла нарушения спеки/философии:\n${JSON.stringify(review.mustFix)}\n\n` +
      `Исправь каждое по спеке docs/ARCHITECTURE_v0.7.md. Гейты после правок прогонит воркфлоу. Перечисли, что исправил.`,
  );
  finalFixes = fixRes;
  const gates = await runGates();
  if (!gates.ok) {
    throw new Error("Финальные исправления сломали гейты:\n" + gates.failures.slice(0, 3000));
  }
  const recheck = await coldReviewer.ask<Verdict>(
    `Правки внесены: ${finalFixes.slice(0, 1500)}\n\nПере-проверь только свои обязательные замечания: ${JSON.stringify(review.mustFix)}`,
  );
  if (recheck.verdict !== "pass") {
    throw new Error("Независимый ревьюер не принял исправления: " + JSON.stringify(recheck.mustFix));
  }
  const sha = await commitStage("Финальные исправления по независимой проверке переезда: " + finalFixes.slice(0, 300));
  log(`Финальные исправления закоммичены: ${sha}`);
}
report({ module: "финал", name: "Независимая проверка", status: "принято", notes: review.notes.join("; ") }, "modules");

// ── Отчёт ────────────────────────────────────────────────────────────────

return {
  conclusion:
    review.verdict === "pass"
      ? `Переезд dio-agent на Go завершён: модули 9.1–9.8 реализованы, гейты (gofmt/vet/test + БД-тесты) зелёные после каждого модуля, независимая проверка нарушений спеки не нашла. TS-код удалён, деплой описан в RUNBOOK. Дальше — ручная приёмка на VPS (Р-критерий этапа 9): живой диалог, память через инструменты, сбой без потери хода.`
      : `Переезд dio-agent на Go завершён: модули 9.1–9.8 реализованы, независимая проверка нашла замечания, исправления внесены и приняты повторно. TS-код удалён. Дальше — ручная приёмка на VPS (Р-критерий этапа 9).`,
  findings: [
    ...review.mustFix.map((f) => ({
      where: "переезд в целом",
      what: f,
      evidence: "найдено независимой проверкой по спеке v0.7, исправлено и принято повторно",
      status: "verified" as const,
      severity: "medium" as const,
    })),
    ...review.notes.map((n) => ({
      where: "переезд в целом",
      what: n,
      evidence: "неблокирующее замечание независимой проверки",
      status: "unconfirmed" as const,
      severity: "low" as const,
    })),
  ],
  verified: [
    "после каждого модуля: gofmt -l . (пустой вывод), go build ./..., go vet ./..., go test ./... (без БД), go test -tags db ./... (Postgres из docker-compose)",
    "каждый модуль принят независимым приёмщиком против спеки v0.7 и CHECKLIST",
    "9.7: сценарии устойчивости §21.1 и главный цикл §1 на скриптованной модели, тег db",
    "финальная независимая проверка инвариантов §3 по коду всего диффа",
    "коммиты по модулям после зелёных гейтов",
  ],
  notCovered: [
    "живые вызовы модели (OpenCode Go/OpenRouter): forced tool-calling и structured output проверены только скриптованной офлайн-моделью — нужен живой смоук",
    "ручная приёмка на VPS (Р-критерий этапа 9): живой диалог, typing, поведение по BEHAVIOR_SCENARIOS",
    "мутационное тестирование Go — решение отложено в бэклог CHECKLIST",
  ],
};
