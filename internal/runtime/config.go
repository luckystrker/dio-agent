// Загрузка конфигурации из переменных окружения.
//
// Список переменных сверён с .env.example и TS-источником (src/runtime/config.ts,
// поведение-справочник этапа 8): обязательный DATABASE_URL; перечислимые
// LOG_LEVEL/NODE_ENV; Telegram (токен/dry-run/api base), модель (endpoint,
// ключ, имя + числовые лимиты) и векторизатор (EMBEDDING_*: endpoint, ключ,
// имя) — группами.
//
// Валидация при старте строгая (требование 9.1): неизвестное значение
// перечислимой переменной, невалидное число или противоречивая комбинация —
// явная ошибка запуска, а не тихое значение по умолчанию. Переменные,
// которые приложение не знает (например POSTGRES_* — их читает
// docker-compose), игнорируются: systemd и контейнер добавляют свои
// переменные, падать из-за них нельзя.
package runtime

import (
	"time"
	_ "time/tzdata" // IANA-зоны встроены в бинарник: AGENT_TIMEZONE не зависит от зон хоста

	"fmt"
	"github.com/luckystrker/dio-agent/internal/web"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LogLevel — уровень логирования (позже перейдёт в структурный логгер).
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// NodeEnv — окружение запуска (влияет только на диагностику).
type NodeEnv string

const (
	NodeEnvDevelopment NodeEnv = "development"
	NodeEnvProduction  NodeEnv = "production"
	NodeEnvTest        NodeEnv = "test"
)

// ModelSettings — модельный адаптер (этап 1.6 TS; в Go — провайдер ago, §17).
// Все поля обязательны: модель либо настроена целиком, либо выключена.
type ModelSettings struct {
	Endpoint        string
	APIKey          string
	Model           string
	TimeoutMs       int
	MaxRetries      int
	MaxOutputTokens int
	DailyCallsLimit int // дневной лимит модельных вызовов (§16)
	// BackgroundCallsLimit — дневной лимит пула background (§16); 0 — как
	// DailyCallsLimit. Отдельный потолок не даёт фоновой жизни (консолидации,
	// exploration, инициативы) выжечь весь день и оставить прямые разговоры
	// без бюджета (инцидент 2026-10-31).
	BackgroundCallsLimit int
	HistoryMessages      int // недавний разговор в контексте хода (§11.1 п.5)
	HistoryMaxChars      int // потолок суммарного объёма истории
	ToolCallsLimit       int // лимит обращений к инструментам за ход (§11.2, §17)
	// MemoryPrefetch — карточка памяти собирается харнессом в панель состояния,
	// и на разговорном ходе с целой историей инструменты памяти не выдаются
	// (§11.1, решение 2026-11-02). false — откат к прежнему поведению: карточки
	// нет, инструменты всегда, forced-вызов на прямом ходе.
	MemoryPrefetch bool
}

// EmbeddingSettings — векторизатор для семантического поиска памяти
// (§10.4 п.4): OpenAI-совместимый endpoint (POST {endpoint}/embeddings).
// Все три поля обязательны вместе: векторизатор либо настроен целиком,
// либо семантический поиск выключен (full-text остаётся).
type EmbeddingSettings struct {
	Endpoint  string
	APIKey    string
	Model     string
	TimeoutMs int
}

// TelegramSettings — доступ к Bot API. Telegram включён, если задан токен
// (живой режим) или dry-run (локальная отладка без токена).
type TelegramSettings struct {
	BotToken string
	APIBase  string
	DryRun   bool
}

// Config — конфигурация приложения. Model == nil — модель не настроена
// (ходы помечаются пропуском с причиной); Telegram == nil — polling и
// доставка выключены; Embedding == nil — семантический поиск памяти выключен.
type Config struct {
	DatabaseURL string
	LogLevel    LogLevel
	NodeEnv     NodeEnv
	// DBMaxConns — потолок соединений пула PostgreSQL (DB_MAX_CONNS, 18.5);
	// LoadConfig всегда заполняет поле (дефолт 8), 0 — незаполненная группа.
	DBMaxConns int
	// MetricsPort — порт HTTP-метрик на 127.0.0.1 (METRICS_PORT, этап 31);
	// 0 — сервер выключен (переменная не задана или пуста).
	MetricsPort int
	// Timezone — часовой пояс меток времени истории и панели состояния
	// (AGENT_TIMEZONE, IANA, по умолчанию Europe/Moscow). LoadConfig всегда
	// заполняет поле; nil — конфиг собран не через LoadConfig.
	Timezone *time.Location
	// FFmpegPath — путь к ffmpeg для раскадровки гифок/кружков (FFMPEG_PATH,
	// этап 35): явный путь, иначе PATH, иначе $HOME/.local/bin/ffmpeg.
	// Пусто — раскадровка выключена, видео идёт превью-кадром.
	FFmpegPath string
	// ModelDebugDumpDir — каталог дампа модельных вызовов (MODEL_DEBUG_DUMP_DIR,
	// этап 40.1). Пусто — дамп выключен (обычный режим).
	ModelDebugDumpDir string
	Model             *ModelSettings
	Telegram          *TelegramSettings
	Embedding         *EmbeddingSettings
	Web               *WebSettings
}

// WebSettings — веб-инструменты exploration (§15/§16): поиск опционален
// (off|ddg|tavily; off — веб выключен целиком, выражения «поиск выключен,
// а fetch включён» не существует), fetch-слой всегда с guard-лимитами.
type WebSettings struct {
	Provider         string // off | ddg | tavily
	APIKey           string // только tavily
	MaxResults       int    // результатов поиска на вызов
	FetchMaxBytes    int64  // потолок байт одного fetch
	FetchTimeoutMs   int
	MaxSearchPerTask int // бюджет поисков одной explore-задачи (§15/§16)
	MaxFetchPerTask  int // бюджет загрузок одной explore-задачи
}

// Getenv — функция доступа к окружению; возвращает (значение, была-ли-задана).
// Разделение «не задана» и «пустая строка» сохраняет поведение TS-версии.
type Getenv func(key string) (string, bool)

// OSEnv — Getenv поверх реального окружения процесса.
func OSEnv(key string) (string, bool) { return os.LookupEnv(key) }

var logLevels = []LogLevel{LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError}
var nodeEnvs = []NodeEnv{NodeEnvDevelopment, NodeEnvProduction, NodeEnvTest}

// LoadConfig читает и валидирует конфигурацию. Любая ошибка — явная причина
// не запускать процесс (требование 9.1).
func LoadConfig(env Getenv) (Config, error) {
	databaseURL := strings.TrimSpace(lookupOrEmpty(env, "DATABASE_URL"))
	if databaseURL == "" {
		return Config{}, fmt.Errorf(
			"DATABASE_URL не задан. Скопируй .env.example в .env и заполни (PostgreSQL поднимается через docker compose)")
	}

	logLevel, err := parseEnum(env, "LOG_LEVEL", LogLevelInfo, logLevels)
	if err != nil {
		return Config{}, err
	}

	nodeEnv, err := parseEnum(env, "NODE_ENV", NodeEnvDevelopment, nodeEnvs)
	if err != nil {
		return Config{}, err
	}

	// Пул соединений (18.5): потолок явный, дефолт pgx (max(4, NumCPU)) растёт
	// с машиной и не контролируется оператором.
	dbMaxConns, err := positiveInt(env, "DB_MAX_CONNS", DefaultMaxConns)
	if err != nil {
		return Config{}, err
	}

	model, err := parseModelSettings(env)
	if err != nil {
		return Config{}, err
	}

	telegram, err := parseTelegramSettings(env)
	if err != nil {
		return Config{}, err
	}

	embedding, err := parseEmbeddingSettings(env)
	if err != nil {
		return Config{}, err
	}

	web, err := parseWebSettings(env)
	if err != nil {
		return Config{}, err
	}

	metricsPort, err := parseMetricsPort(env)
	if err != nil {
		return Config{}, err
	}

	timezone, err := parseTimezone(env)
	if err != nil {
		return Config{}, err
	}

	return Config{
		DatabaseURL: databaseURL,
		LogLevel:    logLevel,
		NodeEnv:     nodeEnv,
		DBMaxConns:  dbMaxConns,
		MetricsPort: metricsPort,
		Timezone:    timezone,
		FFmpegPath:  parseFFmpegPath(env),
		// Дамп модельных вызовов (40.1): не задан или пуст — выключен.
		ModelDebugDumpDir: strings.TrimSpace(lookupOrEmpty(env, "MODEL_DEBUG_DUMP_DIR")),
		Model:             model,
		Telegram:          telegram,
		Embedding:         embedding,
		Web:               web,
	}, nil
}

// DefaultTimezone — часовой пояс персонажа по умолчанию (AGENT_TIMEZONE).
const DefaultTimezone = "Europe/Moscow"

// parseFFmpegPath разрешает ffmpeg для раскадровки (этап 35): FFMPEG_PATH —
// явный путь; пусто — поиск в PATH, затем $HOME/.local/bin/ffmpeg. Найденный
// файл должен быть обычным исполняемым файлом. Несуществующий/неисполняемый
// путь — раскадровка выключена (пусто), а не ошибка запуска: без ffmpeg
// кружки/гифки живут превью-кадром.
func parseFFmpegPath(env Getenv) string {
	if raw := strings.TrimSpace(lookupOrEmpty(env, "FFMPEG_PATH")); raw != "" {
		if executableFile(raw) {
			return raw
		}
		return ""
	}
	if found := lookPathIn(lookupOrEmpty(env, "PATH"), "ffmpeg"); found != "" {
		return found
	}
	if home := strings.TrimSpace(lookupOrEmpty(env, "HOME")); home != "" {
		candidate := filepath.Join(home, ".local", "bin", "ffmpeg")
		if executableFile(candidate) {
			return candidate
		}
	}
	return ""
}

// lookPathIn — поиск исполняемого файла в каталогах PATH из окружения процесса
// (exec.LookPath читал бы реальный os.Getenv, а конфиг должен быть проверяем
// с подменённым окружением).
func lookPathIn(pathEnv, name string) string {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if executableFile(candidate) {
			return candidate
		}
	}
	return ""
}

// executableFile — обычный файл с битом исполнения.
func executableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// parseMetricsPort читает порт HTTP-метрик (METRICS_PORT, этап 31): не задан
// или пуст — сервер выключен (0, не ошибка); иное — целое 1..65535, любые
// отклонения останавливают запуск (оператор не должен гадать, на каком порту
// слушает процесс).
func parseMetricsPort(env Getenv) (int, error) {
	raw := strings.TrimSpace(lookupOrEmpty(env, "METRICS_PORT"))
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 || parsed > 65535 {
		return 0, fmt.Errorf("METRICS_PORT=%q должно быть целым числом 1..65535 (пусто — сервер метрик выключен)", raw)
	}
	return parsed, nil
}

// parseTimezone читает IANA-зону меток времени (AGENT_TIMEZONE): не задана —
// Europe/Moscow; неизвестная зона останавливает запуск, а не подменяется
// молча (метки истории должны быть однозначны).
func parseTimezone(env Getenv) (*time.Location, error) {
	name := strings.TrimSpace(lookupWithDefault(env, "AGENT_TIMEZONE", DefaultTimezone))
	if name == "" {
		name = DefaultTimezone
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("AGENT_TIMEZONE=%q: неизвестная IANA-зона (%v)", name, err)
	}
	return loc, nil
}

// parseWebSettings читает группу WEB_*: по умолчанию веб выключен (§15/§16 —
// включение осознанное); ключ уместен только у провайдеров с ключами.
func parseWebSettings(env Getenv) (*WebSettings, error) {
	provider := strings.TrimSpace(lookupOrEmpty(env, "WEB_SEARCH_PROVIDER"))
	if provider == "" {
		provider = web.ProviderOff
	}
	apiKey := strings.TrimSpace(lookupOrEmpty(env, "WEB_SEARCH_API_KEY"))
	maxResults, err := positiveInt(env, "WEB_SEARCH_MAX_RESULTS", 5)
	if err != nil {
		return nil, err
	}
	fetchBytes := int64(0)
	if lookupOrEmpty(env, "WEB_FETCH_MAX_BYTES") != "" {
		n, err := positiveInt(env, "WEB_FETCH_MAX_BYTES", int(web.DefaultMaxBytes))
		if err != nil {
			return nil, err
		}
		fetchBytes = int64(n)
	}
	fetchTimeoutMs, err := positiveInt(env, "WEB_FETCH_TIMEOUT_MS", int(web.DefaultTimeout/time.Millisecond))
	if err != nil {
		return nil, err
	}
	maxSearch, err := positiveInt(env, "WEB_MAX_SEARCH_PER_TASK", web.DefaultMaxSearchPerTask)
	if err != nil {
		return nil, err
	}
	maxFetch, err := positiveInt(env, "WEB_MAX_FETCH_PER_TASK", web.DefaultMaxFetchPerTask)
	if err != nil {
		return nil, err
	}

	switch provider {
	case web.ProviderOff:
		if apiKey != "" {
			return nil, fmt.Errorf("WEB_SEARCH_API_KEY задан при выключенном вебе: поставь WEB_SEARCH_PROVIDER или убери ключ")
		}
	case web.ProviderDDG, web.ProviderTavily:
		if provider == web.ProviderTavily && apiKey == "" {
			return nil, fmt.Errorf("провайдер поиска tavily требует WEB_SEARCH_API_KEY")
		}
	default:
		return nil, fmt.Errorf("WEB_SEARCH_PROVIDER=%q не из известных (off|ddg|tavily)", provider)
	}
	return &WebSettings{
		Provider:         provider,
		APIKey:           apiKey,
		MaxResults:       maxResults,
		FetchMaxBytes:    fetchBytes,
		FetchTimeoutMs:   fetchTimeoutMs,
		MaxSearchPerTask: maxSearch,
		MaxFetchPerTask:  maxFetch,
	}, nil
}

// parseEmbeddingSettings читает группу EMBEDDING_*: три переменные вместе или
// ни одной (паритет с модельной группой); таймаут — отдельная числовая.
func parseEmbeddingSettings(env Getenv) (*EmbeddingSettings, error) {
	endpoint := strings.TrimSpace(lookupOrEmpty(env, "EMBEDDING_ENDPOINT"))
	apiKey := strings.TrimSpace(lookupOrEmpty(env, "EMBEDDING_API_KEY"))
	model := strings.TrimSpace(lookupOrEmpty(env, "EMBEDDING_MODEL"))

	present := 0
	for _, v := range []string{endpoint, apiKey, model} {
		if v != "" {
			present++
		}
	}
	if present == 0 {
		return nil, nil // векторизатор не настроен — семантический поиск выключен
	}
	if present < 3 {
		return nil, fmt.Errorf(
			"векторизатор настроен частично (%d из 3): нужны EMBEDDING_ENDPOINT, EMBEDDING_API_KEY и EMBEDDING_MODEL вместе — или уберите все три", present)
	}

	timeoutMs, err := positiveInt(env, "EMBEDDING_TIMEOUT_MS", 30_000)
	if err != nil {
		return nil, err
	}

	return &EmbeddingSettings{
		Endpoint:  endpoint,
		APIKey:    apiKey,
		Model:     model,
		TimeoutMs: timeoutMs,
	}, nil
}

func parseTelegramSettings(env Getenv) (*TelegramSettings, error) {
	botToken := strings.TrimSpace(lookupOrEmpty(env, "TELEGRAM_BOT_TOKEN"))
	apiBase := strings.TrimSpace(lookupOrEmpty(env, "TELEGRAM_API_BASE"))
	if apiBase == "" {
		apiBase = "https://api.telegram.org"
	}

	dryRunRaw := strings.ToLower(strings.TrimSpace(lookupWithDefault(env, "TELEGRAM_DRY_RUN", "false")))
	if dryRunRaw != "true" && dryRunRaw != "false" {
		return nil, fmt.Errorf(`TELEGRAM_DRY_RUN должен быть "true" или "false", получено "%s"`, dryRunRaw)
	}
	dryRun := dryRunRaw == "true"

	if !dryRun && botToken == "" {
		return nil, nil // Telegram не настроен — polling и доставка выключены
	}
	if dryRun && botToken != "" {
		return nil, fmt.Errorf(
			"TELEGRAM_DRY_RUN=true вместе с TELEGRAM_BOT_TOKEN не имеет смысла: убери токен или выключи dry-run")
	}
	return &TelegramSettings{BotToken: botToken, APIBase: apiBase, DryRun: dryRun}, nil
}

func parseModelSettings(env Getenv) (*ModelSettings, error) {
	endpoint := strings.TrimSpace(lookupOrEmpty(env, "MODEL_ENDPOINT"))
	apiKey := strings.TrimSpace(lookupOrEmpty(env, "MODEL_API_KEY"))
	model := strings.TrimSpace(lookupOrEmpty(env, "MODEL_MODEL"))

	present := 0
	for _, v := range []string{endpoint, apiKey, model} {
		if v != "" {
			present++
		}
	}
	if present == 0 {
		return nil, nil // модель не настроена — ходы будут пропускаться с причиной
	}
	if present < 3 {
		return nil, fmt.Errorf(
			"модель настроена частично (%d из 3): нужны MODEL_ENDPOINT, MODEL_API_KEY и MODEL_MODEL вместе — или уберите все три", present)
	}

	timeoutMs, err := positiveInt(env, "MODEL_TIMEOUT_MS", 60_000)
	if err != nil {
		return nil, err
	}
	// Ретраи провайдера внутри одного модельного вызова оплачиваются, но
	// резервируется один вызов (§16) — по умолчанию выключены (0), повтор
	// делает worker с длинным backoff (M10). Ненулевое значение оператор
	// ставит сознательно.
	maxRetries, err := nonNegativeInt(env, "MODEL_MAX_RETRIES", 0)
	if err != nil {
		return nil, err
	}
	maxOutputTokens, err := positiveInt(env, "MODEL_MAX_OUTPUT_TOKENS", 1000)
	if err != nil {
		return nil, err
	}
	dailyCallsLimit, err := positiveInt(env, "MODEL_DAILY_CALLS_LIMIT", 100)
	if err != nil {
		return nil, err
	}
	// Пул фона по умолчанию равен общему лимиту (прежнее поведение):
	// отдельное значение задаётся только явно.
	backgroundCallsLimit, err := positiveInt(env, "MODEL_BACKGROUND_CALLS_LIMIT", dailyCallsLimit)
	if err != nil {
		return nil, err
	}
	historyMessages, err := positiveInt(env, "MODEL_HISTORY_MESSAGES", 1000)
	if err != nil {
		return nil, err
	}
	historyMaxChars, err := positiveInt(env, "MODEL_HISTORY_MAX_CHARS", 200_000)
	if err != nil {
		return nil, err
	}
	toolCallsLimit, err := positiveInt(env, "MODEL_TOOL_CALLS_LIMIT", 5)
	if err != nil {
		return nil, err
	}
	memoryPrefetch, err := boolEnv(env, "MODEL_MEMORY_PREFETCH", true)
	if err != nil {
		return nil, err
	}

	return &ModelSettings{
		Endpoint:             endpoint,
		APIKey:               apiKey,
		Model:                model,
		TimeoutMs:            timeoutMs,
		MaxRetries:           maxRetries,
		MaxOutputTokens:      maxOutputTokens,
		DailyCallsLimit:      dailyCallsLimit,
		BackgroundCallsLimit: backgroundCallsLimit,
		HistoryMessages:      historyMessages,
		HistoryMaxChars:      historyMaxChars,
		ToolCallsLimit:       toolCallsLimit,
		MemoryPrefetch:       memoryPrefetch,
	}, nil
}

// LoadModelSettings читает только группу MODEL_* — для обслуживающих команд
// (replay дампа, 40.2): им нужен endpoint и ключ, а не база и Telegram.
func LoadModelSettings(env Getenv) (*ModelSettings, error) { return parseModelSettings(env) }

// boolEnv читает булеву переменную: не задана или пустая — значение по
// умолчанию (паритет с числовыми: «пусто» не ошибка); иное, кроме true/false
// (регистр и края не важны), — явная ошибка запуска.
func boolEnv(env Getenv, name string, def bool) (bool, error) {
	raw, ok := env(name)
	if !ok {
		return def, nil
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return def, nil
	}
	switch strings.ToLower(value) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf(`%s должен быть "true" или "false", получено "%s"`, name, raw)
}

// parseEnum читает перечислимую переменную: не задана — значение по умолчанию;
// задана неверно (включая пустую строку) — явная ошибка.
func parseEnum[T ~string](env Getenv, name string, def T, allowed []T) (T, error) {
	raw, ok := env(name)
	if !ok {
		return def, nil
	}
	value := T(strings.TrimSpace(raw))
	for _, a := range allowed {
		if value == a {
			return value, nil
		}
	}
	return def, fmt.Errorf("%s=\"%s\" не поддерживается. Ожидаю одно из: %s", name, raw, joinEnums(allowed))
}

// positiveInt читает целое положительное число: не задана или пустая —
// значение по умолчанию (поведение TS-версии); иное — явная ошибка.
func positiveInt(env Getenv, name string, def int) (int, error) {
	raw, ok := env(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return def, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s=\"%s\" должно быть целым положительным числом", name, raw)
	}
	return parsed, nil
}

// nonNegativeInt — целое >= 0; в отличие от positiveInt допускает 0
// (MODEL_MAX_RETRIES=0 — ретраи провайдера выключены).
func nonNegativeInt(env Getenv, name string, def int) (int, error) {
	raw, ok := env(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return def, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s=\"%s\" должно быть целым неотрицательным числом", name, raw)
	}
	return parsed, nil
}

func lookupOrEmpty(env Getenv, name string) string {
	v, _ := env(name)
	return v
}

func lookupWithDefault(env Getenv, name, def string) string {
	if v, ok := env(name); ok {
		return v
	}
	return def
}

func joinEnums[T ~string](values []T) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = string(v)
	}
	return strings.Join(parts, ", ")
}
