// Сборка и жизненный цикл приложения (§5.1, §5.3, §12.2).
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/luckystrker/dio-agent/internal/background"
	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/internal/telegram"
	"github.com/luckystrker/dio-agent/internal/web"
	"github.com/luckystrker/dio-agent/internal/worker"
	"github.com/luckystrker/dio-agent/migrations"
)

// Пути persona-файлов (§7.1; относительно рабочего каталога процесса).
const (
	personaFilePath    = "config/persona.md"
	promptTemplatePath = "config/system-prompt.md"
	// staleSendingAfter — «sending» старше этого возраста принадлежит мёртвому
	// процессу: recovery §12.2 переводит её в unknown (не отправлять вслепую).
	staleSendingAfter = time.Minute
	// stalePendingAfter — pending старше этого возраста возвращается в ход
	// периодическим recovery живого процесса (M5): окна буферов не больше
	// 10 секунд, минута — с запасом.
	stalePendingAfter = time.Minute
)

// DefaultDrainLimit — граница ожидания текущего хода при shutdown (§5.3:
// «ограниченно дождаться текущих операций»); запас поверх таймаута модели.
const DefaultDrainLimit = 90 * time.Second

// Options — сборка приложения. Нулевые точки внедрения означают «собрать из
// Config»; ненулевые подменяют слой целиком (тесты, отладка).
type Options struct {
	Config runtime.Config
	Logger *log.Logger

	// PhotoFetcher — скачивание фото Telegram (vision §6.1); nil — без фото.
	// Приложение строит само, если включён реальный Telegram.
	PhotoFetcher character.PhotoFetcher

	// PersonaFile / PromptTemplate — пути к файлам §7.1 (по умолчанию
	// config/ от рабочей директории; тесты указывают на корень репозитория —
	// молчаливый fallback на дефолтную персону маскировал shipped-файлы,
	// найдено живыми эвалами 2026-10-31).
	PersonaFile    string
	PromptTemplate string

	// Model — модельный конфиг поверх provider ago (§17); тесты подставляют
	// скриптовый дубль. nil — сборка из Config.Model.
	Model *character.ChatConfig
	// Telegram — клиент Bot API; nil — сборка из Config.Telegram (живой клиент
	// или dry-run заглушка).
	Telegram telegram.API
	// Embedder — векторизатор памяти (§10.4); nil — сборка из Config.Embedding.
	Embedder memory.Embedder
	// Now — часы планировщика фона (тесты двигают дни). nil — time.Now.
	Now func() time.Time

	// SingletonWatch — период проверки singleton-соединения (§5.1).
	SingletonWatch time.Duration
	// DrainLimit — граница ожидания текущего хода при shutdown (§5.3).
	DrainLimit time.Duration

	// Настройки циклов (тесты сжимают интервалы; прод — значения по умолчанию).
	Poll       telegram.PollerOptions
	Buffers    func(chatType telegram.ChatType, kind telegram.EventKind) telegram.Windows
	Cognitive  worker.Options
	Delivery   telegram.DeliveryOptions
	Background background.Options
}

func (o Options) logger() *log.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return log.New(os.Stderr, "dio: ", log.LstdFlags)
}

func (o Options) drainLimit() time.Duration {
	if o.DrainLimit > 0 {
		return o.DrainLimit
	}
	return DefaultDrainLimit
}

// App — собранное приложение. Start запускает, Stop останавливает (§5.3).
type App struct {
	opts   Options
	logger *log.Logger

	pool        *runtime.Pool
	single      *runtime.Singleton
	persona     character.Persona
	journal     *character.TurnJournal
	checkpoints *runtime.PgCheckpointStore // чекпоинты задач §12.2 (дренаж-контекст 17.1)
	runner      *character.Runner
	buffers     *telegram.ChatBufferManager
	cognit      *worker.Worker
	sched       *background.Scheduler
	delivery    *telegram.DeliveryLoop
	poller      *telegram.TelegramPoller

	runCtx       context.Context // весь процесс: потеря singleton/фатальная ошибка отменяют
	cancelRun    context.CancelFunc
	intakeCtx    context.Context // приём новых updates (polling)
	cancelIntake context.CancelFunc

	wg      sync.WaitGroup // фоновые циклы
	mu      sync.Mutex
	fatal   error // первая фатальная причина остановки
	started bool
}

// New валидирует сборку без подключения к БД.
func New(opts Options) (*App, error) {
	if opts.Config.DatabaseURL == "" {
		return nil, errors.New("app: пустой DATABASE_URL — приложению нужна база (инвариант 3)")
	}
	return &App{opts: opts, logger: opts.logger()}, nil
}

// Start выполняет startup §5.3: подключить БД → миграции → singleton →
// персона → восстановить незавершённую работу → запустить циклы.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return errors.New("app: повторный Start")
	}
	a.started = true
	a.mu.Unlock()

	a.runCtx, a.cancelRun = context.WithCancel(context.Background())
	a.intakeCtx, a.cancelIntake = context.WithCancel(a.runCtx)

	// --- БД и схема ---
	pool, err := runtime.NewPool(ctx, a.opts.Config.DatabaseURL, a.opts.Config.DBMaxConns)
	if err != nil {
		return a.stopFailed(fmt.Errorf("app: PostgreSQL не подключён: %w", err))
	}
	a.pool = pool
	result, err := runtime.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		return a.stopFailed(fmt.Errorf("app: миграции не применены: %w", err))
	}
	a.logger.Printf("миграции: применено %d, пропущено %d", result.Applied, result.Skipped)

	// --- singleton §5.1 ---
	single, err := runtime.AcquireSingleton(ctx, a.opts.Config.DatabaseURL)
	if err != nil {
		return a.stopFailed(err)
	}
	a.single = single
	a.single.OnLost(func(reason string) {
		a.fatalFail(fmt.Errorf("%s — процесс останавливается (§5.1: без singleton-защиты работу не продолжаем)", reason))
	})
	a.single.StartWatch(a.opts.SingletonWatch)
	a.logger.Printf("singleton получен (pid %d)", single.PID())

	// --- персона (§7.1): файлы — источник истины, отличие создаёт версию ---
	persona, template, err := a.loadPersona(ctx)
	if err != nil {
		return a.stopFailed(err)
	}
	a.persona = persona

	// --- память, журнал, бюджет ---
	// Адаптеры журнала/WAL/чекпоинтов живут на runCtx, а не на пришедшем ctx:
	// сигнал (SIGTERM) отменяет внешний контекст ДО graceful drain (§5.3), и
	// записи дренажа — model_output сгенерированного ответа, WAL, чекпоинты —
	// падали весь остаток хода. runCtx отменяется в Stop уже ПОСЛЕ дренажа.
	storage := runtime.NewPgStreamStorage(a.runCtx, pool)
	a.journal = character.NewTurnJournal(storage, a.logger)
	a.checkpoints = runtime.NewPgCheckpointStore(a.runCtx, pool)

	// --- модельный слой ---
	model := a.opts.Model
	if model == nil && a.opts.Config.Model != nil {
		m := a.opts.Config.Model
		model = character.NewChatConfig(m.Endpoint, m.APIKey, m.Model,
			int64(m.MaxOutputTokens), m.TimeoutMs, m.MaxRetries)
	}
	if model == nil {
		a.logger.Printf("модель не настроена (MODEL_ENDPOINT/API_KEY/MODEL): ходы будут пропускаться")
	}

	toolset, err := a.buildMemory(ctx)
	if err != nil {
		return a.stopFailed(err)
	}

	// --- общий семафор когниции (§5.2) ---
	// Прямой ход worker'а и модельные задачи фона делят один семафор: два
	// ApplyTurn и двойной расход бюджета параллельно невозможны (M1).
	cognition := runtime.NewCognitionLock()

	// --- когнитивный worker §5.2 ---
	assembler := &character.Assembler{
		Mood:            character.NewMoodRepo(),
		Relationships:   character.NewRelationshipRepo(),
		PromptTemplate:  template,
		HistoryMessages: historyMessages(a.opts.Config),
		HistoryMaxChars: historyMaxChars(a.opts.Config),
		Timezone:        a.opts.Config.Timezone,
		Now:             a.opts.Now,
		Memory:          toolset,
		MemoryPrefetch:  memoryPrefetch(a.opts.Config),
		Logger:          a.logger,
	}
	runner := &character.Runner{
		Pool:            pool,
		Journal:         a.journal,
		Budget:          runtime.NewBudgetStore(pool),
		Model:           model,
		DailyCallsLimit: dailyCallsLimit(a.opts.Config),
		ToolCallsLimit:  toolCallsLimit(a.opts.Config),
		Assembler:       assembler,
		Persona:         persona,
		Memory:          toolset,
		MemoryPrefetch:  memoryPrefetch(a.opts.Config),
		Timezone:        a.opts.Config.Timezone,
		Logger:          a.logger,
	}
	a.runner = runner

	// Telegram: приём и доставка (§6, §13).
	api := a.opts.Telegram
	if api == nil {
		api = buildTelegramAPI(a.opts.Config, a.logger)
	}
	if a.opts.PhotoFetcher != nil {
		runner.Photos = a.opts.PhotoFetcher
	} else if tg := a.opts.Config.Telegram; tg != nil && !tg.DryRun && tg.BotToken != "" {
		// Vision §6.1: реальный Telegram — фото качаются; dry-run — ход по тексту.
		runner.Photos = telegram.NewMediaFetcher(tg.BotToken, tg.APIBase)
	}
	var pulse *character.TypingPulse
	if api != nil {
		pulse = character.NewTypingPulse(api, 4*time.Second) // §13.3: ~раз в 4 с
		runner.Typing = pulse
	}

	a.cognit = worker.New(worker.Deps{
		Pool:      pool,
		Runner:    runner,
		Journal:   a.journal,
		Logger:    a.logger,
		Cognition: cognition,
		PendingRecovery: func(ctx context.Context) error {
			_, _, err := a.buffers.RecoverStalePendingOlderThan(ctx, stalePendingAfter)
			return err
		},
	}, a.opts.Cognitive)

	bufferDeps := telegram.BufferDeps{
		Pool:        pool,
		Logger:      a.logger,
		OnTurnReady: func(turnID, chatID int64, kind string) { a.cognit.Wake() },
	}
	a.buffers = telegram.NewChatBufferManager(bufferDeps)
	if a.opts.Buffers != nil {
		a.buffers.Windows = a.opts.Buffers
	}

	if api != nil {
		a.poller = telegram.NewTelegramPoller(pool, api, a.buffers.Add, a.logger, a.opts.Poll)
		if bot, berr := api.GetMe(ctx); berr != nil {
			a.logger.Printf("getMe не удался: reply/mention в группах считаются по прежним правилам: %v", berr)
		} else {
			a.poller.SetBotIdentity(bot)
		}
		a.delivery = telegram.NewDeliveryLoop(pool, telegram.NewOutboxRepo(pool), api, a.logger, a.opts.Delivery)
	} else {
		a.logger.Printf("telegram не настроен (TELEGRAM_BOT_TOKEN/TELEGRAM_DRY_RUN): polling и доставка выключены")
	}

	// --- фон §14 ---
	var webset *web.Toolset
	if w := a.opts.Config.Web; w != nil && w.Provider != "off" {
		fetchBytes := w.FetchMaxBytes
		if fetchBytes <= 0 {
			fetchBytes = web.DefaultMaxBytes
		}
		ws, werr := web.NewToolset(
			web.NewClient(fetchBytes, time.Duration(w.FetchTimeoutMs)*time.Millisecond),
			w.Provider, w.APIKey, w.MaxResults)
		if werr != nil {
			return a.stopFailed(werr)
		}
		webset = ws
		a.logger.Printf("веб-инструменты включены: поиск %s (лимиты %d/%d на задачу, §15/§16)",
			w.Provider, w.MaxSearchPerTask, w.MaxFetchPerTask)
	}

	sched, err := background.New(background.Deps{
		Pool:            pool,
		Journal:         a.journal,
		WAL:             runtime.NewTaskWAL(storage),
		Checkpoints:     a.checkpoints,
		Budget:          runtime.NewBudgetStore(pool),
		Memory:          toolset,
		Web:             webset,
		WebMaxSearch:    webSearchLimit(a.opts.Config),
		WebMaxFetch:     webFetchLimit(a.opts.Config),
		Persona:         persona,
		PromptTemplate:  template,
		Model:           model,
		DailyCallsLimit: backgroundCallsLimit(a.opts.Config),
		ToolCallsLimit:  toolCallsLimit(a.opts.Config),
		Cognition:       cognition,
		Logger:          a.logger,
		Now:             a.opts.Now,
	}, a.opts.Background)
	if err != nil {
		return a.stopFailed(err)
	}
	a.sched = sched

	// --- recovery §12.2 (до запуска циклов) ---
	chats, events, err := a.buffers.RecoverStalePending(ctx)
	if err != nil {
		return a.stopFailed(fmt.Errorf("app: recovery pending-событий не удался: %w", err))
	}
	if chats > 0 {
		a.logger.Printf("recovery: %d порций чатов (%d событий) возвращены в очередь (§12.2)", chats, events)
	}
	if a.delivery != nil {
		if stale, err := telegram.NewOutboxRepo(pool).RecoverStaleSending(ctx, staleSendingAfter); err != nil {
			return a.stopFailed(fmt.Errorf("app: recovery outbox не удался: %w", err))
		} else if stale > 0 {
			a.logger.Printf("recovery: %d зависших доставок переведены в unknown/cancelled (§12.2)", stale)
		}
	}

	// --- циклы ---
	if a.poller != nil {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			if err := a.poller.Run(a.intakeCtx); err != nil {
				a.fatalFail(fmt.Errorf("polling остановлен: %w", err))
			}
		}()
	}
	if a.delivery != nil {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.delivery.Run(a.runCtx)
		}()
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		if err := a.sched.Run(a.runCtx); err != nil {
			a.fatalFail(fmt.Errorf("планировщик фона остановлен: %w", err))
		}
	}()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		if err := a.cognit.Run(a.runCtx); err != nil {
			a.fatalFail(fmt.Errorf("когнитивный worker остановлен: %w", err))
		}
	}()

	a.logger.Printf("приложение запущено: модель=%s telegram=%v",
		map[bool]string{true: "настроена", false: "не настроена"}[model != nil], a.poller != nil)
	return nil
}

// Run = Start + ожидание завершения + graceful Stop. Ошибка — причина, по
// которой процесс останавливается (занятый singleton, потеря замка, сбой
// polling): вызывающий решает код выхода.
func Run(ctx context.Context, opts Options) error {
	a, err := New(opts)
	if err != nil {
		return err
	}
	if err := a.Start(ctx); err != nil {
		return err
	}

	var cause error
	select {
	case <-ctx.Done():
	case <-a.runCtx.Done():
		a.mu.Lock()
		cause = a.fatal
		a.mu.Unlock()
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), a.opts.drainLimit()+5*time.Second)
	defer cancel()
	a.Stop(stopCtx)
	return cause
}

// Stop — graceful shutdown §5.3: остановить приём новой локальной работы →
// ограниченно дождаться текущих операций (сохраняя доступные результаты) →
// закрыть соединения. Повторный вызов и вызов до Start безопасны.
func (a *App) Stop(ctx context.Context) {
	if !a.started {
		return
	}

	// 1. Остановить приём: polling прекращает новые updates (§5.3).
	a.cancelIntake()
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()

	// 2. Ограниченно дождаться текущего хода (§5.3): worker получает drain —
	// «новой работы не брать, текущий ход доработать», иначе Done недостижим
	// и каждый Stop ждал бы весь лимит (в проде — 90 с с удержанием замка).
	// Drain идёт ДО закрытия буферов (решение 18.4): FlushAll создаёт ходы и
	// будит worker — при обратном порядке каждый stop при живом чате стартовал
	// бы новый модельный ход и держал процесс до лимита. Не дождались — жёсткая
	// отмена. Worker может быть не собран (частичный Start) — ждать тогда нечего.
	if a.cognit != nil {
		a.cognit.StopDrain()
		drainTimer := time.NewTimer(a.opts.drainLimit())
		select {
		case <-a.cognit.Done():
			drainTimer.Stop()
		case <-drainTimer.C:
			a.logger.Printf("shutdown: текущий ход не завершился за %v — отменяю (§5.3)", a.opts.drainLimit())
			a.cancelRun()
		}
	}

	// 3. Сохранить доступные результаты: незакрытые буферы — в ходы (§6.2).
	// Worker уже остановлен: ходы остаются created и лежат в очереди — их
	// подберёт следующий запуск (Wake после выхода worker безвреден, recovery
	// видит created-ходы при старте).
	a.buffers.FlushAll(ctx)

	// 4. Остальные циклы (scheduler, delivery).
	a.cancelRun()
	select {
	case <-done:
	case <-ctx.Done():
	}

	// 5. Соединения.
	if a.pool != nil {
		a.pool.Close()
	}
	if a.single != nil {
		_ = a.single.Release(ctx)
	}
	a.logger.Printf("приложение остановлено")
}

// stopFailed закрывает частично собранное приложение при ошибке startup.
func (a *App) stopFailed(err error) error {
	if a.cancelRun != nil {
		a.cancelRun()
	}
	if a.single != nil {
		_ = a.single.Release(context.Background())
	}
	if a.pool != nil {
		a.pool.Close()
	}
	return err
}

// fatalFail фиксирует первую фатальную причину и отменяет работу процесса
// (потеря singleton §5.1, сбой цикла): оставшиеся циклы останавливаются.
func (a *App) fatalFail(err error) {
	a.mu.Lock()
	if a.fatal == nil {
		a.fatal = err
	}
	stop := a.fatal == err
	a.mu.Unlock()
	if !stop {
		return
	}
	a.logger.Printf("ФАТАЛЬНО: %v", err)
	a.cancelRun()
}

// loadPersona — активная персона; config/persona.md при наличии синхронизируется
// (отличие файла создаёт версию, §7.1), config/system-prompt.md — каркас промпта.
// Пути берутся из опций (дефолт — от рабочей директории процесса).
func (a *App) loadPersona(ctx context.Context) (character.Persona, string, error) {
	repo := character.NewPersonaRepo()
	personaPath := a.opts.PersonaFile
	if personaPath == "" {
		personaPath = personaFilePath
	}
	templatePath := a.opts.PromptTemplate
	if templatePath == "" {
		templatePath = promptTemplatePath
	}

	raw, err := os.ReadFile(personaPath)
	switch {
	case err == nil:
		source, perr := character.ParsePersonaFile(string(raw))
		if perr != nil {
			return character.Persona{}, "", fmt.Errorf("app: %s: %w", personaPath, perr)
		}
		if _, _, uerr := repo.EnsureFromSource(ctx, a.pool, source); uerr != nil {
			return character.Persona{}, "", fmt.Errorf("app: персона файла не синхронизирована: %w", uerr)
		}
	case errors.Is(err, os.ErrNotExist):
		// Файла нет — дефолтная персона версии 1 (паритет 9.4).
	default:
		return character.Persona{}, "", fmt.Errorf("app: %s не читается: %w", personaPath, err)
	}

	persona, err := repo.Active(ctx, a.pool)
	if err != nil {
		return character.Persona{}, "", fmt.Errorf("app: активная персона не получена: %w", err)
	}

	var template string
	if raw, err := os.ReadFile(templatePath); err == nil {
		template = string(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return character.Persona{}, "", fmt.Errorf("app: %s не читается: %w", promptTemplatePath, err)
	}
	return persona, template, nil
}

// buildMemory — инструментарий памяти (§10.4): векторизатор из Options или
// конфигурации EMBEDDING_*; не настроен — семантика честно недоступна.
func (a *App) buildMemory(ctx context.Context) (*memory.Toolset, error) {
	embedder := a.opts.Embedder
	if embedder == nil && a.opts.Config.Embedding != nil {
		e := a.opts.Config.Embedding
		embedder = memory.NewOpenAIEmbedder(e.Endpoint, e.APIKey, e.Model,
			time.Duration(e.TimeoutMs)*time.Millisecond, nil)
	}
	toolset, err := memory.NewToolset(a.pool, &memory.Searcher{Embedder: embedder, Items: memory.Items{}})
	if err != nil {
		return nil, fmt.Errorf("app: инструментарий памяти не собран: %w", err)
	}
	return toolset, nil
}

// buildTelegramAPI — клиент Bot API по конфигурации; dry-run — заглушка,
// пишущая отправки в лог (живой локальный прогон — с настоящим токеном).
// logger — логгер приложения: dry-run без токена виден в журнале процесса.
func buildTelegramAPI(cfg runtime.Config, logger *log.Logger) telegram.API {
	if cfg.Telegram == nil {
		return nil
	}
	if cfg.Telegram.DryRun {
		if logger == nil {
			logger = log.New(os.Stderr, "dio: ", log.LstdFlags)
		}
		return &dryRunAPI{logger: logger}
	}
	return telegram.NewClient(cfg.Telegram.BotToken, cfg.Telegram.APIBase, nil)
}

func dailyCallsLimit(cfg runtime.Config) int64 {
	if cfg.Model == nil {
		return 0
	}
	return int64(cfg.Model.DailyCallsLimit)
}

// backgroundCallsLimit — дневной потолок пула background (§16). Отдельное
// значение (MODEL_BACKGROUND_CALLS_LIMIT) не даёт фону выжечь день целиком:
// прямые разговоры должны остаться с бюджетом; пусто — общий лимит.
func backgroundCallsLimit(cfg runtime.Config) int64 {
	if cfg.Model == nil {
		return 0
	}
	if cfg.Model.BackgroundCallsLimit > 0 {
		return int64(cfg.Model.BackgroundCallsLimit)
	}
	return int64(cfg.Model.DailyCallsLimit)
}

func webSearchLimit(cfg runtime.Config) int {
	if cfg.Web == nil || cfg.Web.MaxSearchPerTask <= 0 {
		return web.DefaultMaxSearchPerTask
	}
	return cfg.Web.MaxSearchPerTask
}

func webFetchLimit(cfg runtime.Config) int {
	if cfg.Web == nil || cfg.Web.MaxFetchPerTask <= 0 {
		return web.DefaultMaxFetchPerTask
	}
	return cfg.Web.MaxFetchPerTask
}

func toolCallsLimit(cfg runtime.Config) int {
	if cfg.Model == nil {
		return 0
	}
	return cfg.Model.ToolCallsLimit
}

// memoryPrefetch — срез 2 (решение 2026-11-02): карточка памяти в панели
// вместо принудительного поиска. Модель не настроена — значение из конфига
// (по умолчанию true) не важно: инструментов памяти всё равно нет.
func memoryPrefetch(cfg runtime.Config) bool {
	if cfg.Model == nil {
		return true
	}
	return cfg.Model.MemoryPrefetch
}

func historyMessages(cfg runtime.Config) int {
	if cfg.Model == nil || cfg.Model.HistoryMessages <= 0 {
		return 1000
	}
	return cfg.Model.HistoryMessages
}

func historyMaxChars(cfg runtime.Config) int {
	if cfg.Model == nil || cfg.Model.HistoryMaxChars <= 0 {
		return 200_000
	}
	return cfg.Model.HistoryMaxChars
}
