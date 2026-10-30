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
	"fmt"
	"os"
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
	HistoryMessages int // недавний разговор в контексте хода (§11.1 п.5)
	HistoryMaxChars int // потолок суммарного объёма истории
	ToolCallsLimit  int // лимит обращений к инструментам за ход (§11.2, §17)
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
	Model       *ModelSettings
	Telegram    *TelegramSettings
	Embedding   *EmbeddingSettings
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

	return Config{
		DatabaseURL: databaseURL,
		LogLevel:    logLevel,
		NodeEnv:     nodeEnv,
		Model:       model,
		Telegram:    telegram,
		Embedding:   embedding,
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
	maxRetries, err := positiveInt(env, "MODEL_MAX_RETRIES", 1)
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

	return &ModelSettings{
		Endpoint:        endpoint,
		APIKey:          apiKey,
		Model:           model,
		TimeoutMs:       timeoutMs,
		MaxRetries:      maxRetries,
		MaxOutputTokens: maxOutputTokens,
		DailyCallsLimit: dailyCallsLimit,
		HistoryMessages: historyMessages,
		HistoryMaxChars: historyMaxChars,
		ToolCallsLimit:  toolCallsLimit,
	}, nil
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
