package runtime

import (
	"strings"
	"testing"
)

// Тесты повторяют контракт TS-версии (src/runtime/config.ts) — справочник
// поведения: обязательный DATABASE_URL, перечислимые уровни/окружения,
// модель и Telegram включаются только целиком.

func envOf(pairs map[string]string) Getenv {
	return func(key string) (string, bool) {
		v, ok := pairs[key]
		return v, ok
	}
}

func TestLoadConfigMinimal(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "postgres://dio:dio@localhost:5433/dio"}))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if cfg.DatabaseURL != "postgres://dio:dio@localhost:5433/dio" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.LogLevel != LogLevelInfo {
		t.Errorf("LogLevel по умолчанию = %q, хочу info", cfg.LogLevel)
	}
	if cfg.NodeEnv != NodeEnvDevelopment {
		t.Errorf("NodeEnv по умолчанию = %q, хочу development", cfg.NodeEnv)
	}
	if cfg.Model != nil {
		t.Errorf("модель без переменных должна быть выключена, получено %+v", cfg.Model)
	}
	if cfg.Telegram != nil {
		t.Errorf("telegram без токена и dry-run должен быть выключен, получено %+v", cfg.Telegram)
	}
}

func TestLoadConfigDatabaseURLRequired(t *testing.T) {
	for _, raw := range []struct {
		name string
		env  map[string]string
	}{
		{"не задан", map[string]string{}},
		{"пустой", map[string]string{"DATABASE_URL": "   "}},
	} {
		if _, err := LoadConfig(envOf(raw.env)); err == nil {
			t.Errorf("%s: ожидаю явную ошибку запуска", raw.name)
		} else if !strings.Contains(err.Error(), "DATABASE_URL") {
			t.Errorf("%s: ошибка должна называть переменную, получено: %v", raw.name, err)
		}
	}
}

func TestLoadConfigEnumValidation(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string // подстрока ошибки
	}{
		{
			name: "неизвестный уровень логирования",
			env:  map[string]string{"DATABASE_URL": "x", "LOG_LEVEL": "verbose"},
			want: `LOG_LEVEL="verbose" не поддерживается`,
		},
		{
			name: "пустой LOG_LEVEL не заменяется умолчанием",
			env:  map[string]string{"DATABASE_URL": "x", "LOG_LEVEL": ""},
			want: `LOG_LEVEL="" не поддерживается`,
		},
		{
			name: "неизвестное окружение",
			env:  map[string]string{"DATABASE_URL": "x", "NODE_ENV": "staging"},
			want: `NODE_ENV="staging" не поддерживается`,
		},
	}
	for _, c := range cases {
		if _, err := LoadConfig(envOf(c.env)); err == nil {
			t.Errorf("%s: ожидаю явную ошибку", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: ошибка %q не содержит %q", c.name, err, c.want)
		}
	}
}

func TestLoadConfigEnumValidValues(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		cfg, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "LOG_LEVEL": level}))
		if err != nil || cfg.LogLevel != LogLevel(level) {
			t.Errorf("LOG_LEVEL=%s: err=%v level=%q", level, err, cfg.LogLevel)
		}
	}
	for _, envName := range []string{"development", "production", "test"} {
		cfg, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "NODE_ENV": envName}))
		if err != nil || cfg.NodeEnv != NodeEnv(envName) {
			t.Errorf("NODE_ENV=%s: err=%v env=%q", envName, err, cfg.NodeEnv)
		}
	}
}

func TestLoadConfigTelegram(t *testing.T) {
	t.Run("живой режим по токену", func(t *testing.T) {
		cfg, err := LoadConfig(envOf(map[string]string{
			"DATABASE_URL":       "x",
			"TELEGRAM_BOT_TOKEN": "123:abc",
			"TELEGRAM_API_BASE":  "https://example.com",
		}))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if cfg.Telegram == nil || cfg.Telegram.DryRun || cfg.Telegram.BotToken != "123:abc" {
			t.Fatalf("telegram = %+v", cfg.Telegram)
		}
		if cfg.Telegram.APIBase != "https://example.com" {
			t.Errorf("APIBase = %q", cfg.Telegram.APIBase)
		}
	})

	t.Run("api base по умолчанию", func(t *testing.T) {
		cfg, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "TELEGRAM_BOT_TOKEN": "t"}))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if cfg.Telegram.APIBase != "https://api.telegram.org" {
			t.Errorf("APIBase = %q, хочу дефолт", cfg.Telegram.APIBase)
		}
	})

	t.Run("dry-run без токена", func(t *testing.T) {
		cfg, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "TELEGRAM_DRY_RUN": "TRUE"}))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if cfg.Telegram == nil || !cfg.Telegram.DryRun || cfg.Telegram.BotToken != "" {
			t.Fatalf("telegram = %+v, хочу dry-run без токена", cfg.Telegram)
		}
	})

	t.Run("dry-run вместе с токеном — ошибка", func(t *testing.T) {
		_, err := LoadConfig(envOf(map[string]string{
			"DATABASE_URL":       "x",
			"TELEGRAM_DRY_RUN":   "true",
			"TELEGRAM_BOT_TOKEN": "123:abc",
		}))
		if err == nil || !strings.Contains(err.Error(), "TELEGRAM_DRY_RUN=true вместе с TELEGRAM_BOT_TOKEN") {
			t.Fatalf("ожидаю ошибку противоречия, получено: %v", err)
		}
	})

	t.Run("невалидный dry-run — ошибка", func(t *testing.T) {
		_, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "TELEGRAM_DRY_RUN": "yes"}))
		if err == nil || !strings.Contains(err.Error(), `TELEGRAM_DRY_RUN должен быть "true" или "false"`) {
			t.Fatalf("ожидаю ошибку валидации, получено: %v", err)
		}
	})
}

func TestLoadConfigModel(t *testing.T) {
	full := map[string]string{
		"DATABASE_URL":   "x",
		"MODEL_ENDPOINT": "https://api.example.com/v1",
		"MODEL_API_KEY":  "key",
		"MODEL_MODEL":    "gpt-x",
	}

	t.Run("полная настройка с числовыми лимитами", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		env["MODEL_TIMEOUT_MS"] = "2500"
		env["MODEL_HISTORY_MESSAGES"] = "500"
		cfg, err := LoadConfig(envOf(env))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		m := cfg.Model
		if m == nil {
			t.Fatal("модель должна быть настроена")
		}
		if m.TimeoutMs != 2500 || m.HistoryMessages != 500 {
			t.Errorf("числовые значения не применились: %+v", m)
		}
		if m.MaxRetries != 1 || m.MaxOutputTokens != 1000 || m.DailyCallsLimit != 100 || m.HistoryMaxChars != 200_000 {
			t.Errorf("умолчания не совпали: %+v", m)
		}
	})

	t.Run("частичная настройка — ошибка", func(t *testing.T) {
		_, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "MODEL_ENDPOINT": "https://x", "MODEL_API_KEY": "k"}))
		if err == nil || !strings.Contains(err.Error(), "модель настроена частично (2 из 3)") {
			t.Fatalf("ожидаю ошибку частичной настройки, получено: %v", err)
		}
	})

	t.Run("невалидные числа — ошибка с именем переменной", func(t *testing.T) {
		for name, value := range map[string]string{
			"MODEL_TIMEOUT_MS":        "abc",
			"MODEL_MAX_RETRIES":       "0",
			"MODEL_MAX_OUTPUT_TOKENS": "-5",
			"MODEL_DAILY_CALLS_LIMIT": "1.5",
		} {
			env := map[string]string{}
			for k, v := range full {
				env[k] = v
			}
			env[name] = value
			_, err := LoadConfig(envOf(env))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: ожидаю ошибку с именем переменной, получено: %v", name, value, err)
			}
		}
	})

	t.Run("пул фона: отдельный лимит и умолчание от общего", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		cfg, err := LoadConfig(envOf(env))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if cfg.Model.BackgroundCallsLimit != cfg.Model.DailyCallsLimit {
			t.Errorf("без MODEL_BACKGROUND_CALLS_LIMIT пул фона берёт общий лимит: %d != %d",
				cfg.Model.BackgroundCallsLimit, cfg.Model.DailyCallsLimit)
		}
		env["MODEL_BACKGROUND_CALLS_LIMIT"] = "400"
		cfg, err = LoadConfig(envOf(env))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if cfg.Model.BackgroundCallsLimit != 400 {
			t.Errorf("отдельный лимит пула фона не применился: %d", cfg.Model.BackgroundCallsLimit)
		}
	})

	t.Run("пустое число = умолчание (паритет с TS)", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		env["MODEL_MAX_RETRIES"] = "   "
		cfg, err := LoadConfig(envOf(env))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if cfg.Model.MaxRetries != 1 {
			t.Errorf("пустое значение должно давать умолчание, получено %d", cfg.Model.MaxRetries)
		}
	})
}

// Векторизатор семантического поиска (этап 9.3, §10.4): группа EMBEDDING_*
// включается только целиком; не настроен — семантика выключена.
func TestLoadConfigEmbedding(t *testing.T) {
	full := map[string]string{
		"DATABASE_URL":       "x",
		"EMBEDDING_ENDPOINT": "https://api.example.com/v1",
		"EMBEDDING_API_KEY":  "key",
		"EMBEDDING_MODEL":    "text-embedding-3-small",
	}

	t.Run("полная настройка с таймаутом", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		env["EMBEDDING_TIMEOUT_MS"] = "5000"
		cfg, err := LoadConfig(envOf(env))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if cfg.Embedding == nil {
			t.Fatal("векторизатор должен быть настроен")
		}
		if cfg.Embedding.Endpoint != "https://api.example.com/v1" || cfg.Embedding.Model != "text-embedding-3-small" || cfg.Embedding.TimeoutMs != 5000 {
			t.Errorf("значения не применились: %+v", cfg.Embedding)
		}
	})

	t.Run("умолчание таймаута", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		cfg, err := LoadConfig(envOf(env))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if cfg.Embedding.TimeoutMs != 30_000 {
			t.Errorf("таймаут по умолчанию = %d, хочу 30000", cfg.Embedding.TimeoutMs)
		}
	})

	t.Run("не настроен — семантика выключена", func(t *testing.T) {
		cfg, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x"}))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if cfg.Embedding != nil {
			t.Errorf("без EMBEDDING_* векторизатор должен быть выключен, получено %+v", cfg.Embedding)
		}
	})

	t.Run("частичная настройка — ошибка", func(t *testing.T) {
		_, err := LoadConfig(envOf(map[string]string{
			"DATABASE_URL":       "x",
			"EMBEDDING_ENDPOINT": "https://x",
			"EMBEDDING_API_KEY":  "k",
		}))
		if err == nil || !strings.Contains(err.Error(), "векторизатор настроен частично (2 из 3)") {
			t.Fatalf("ожидаю ошибку частичной настройки, получено: %v", err)
		}
	})

	t.Run("невалидный таймаут — ошибка с именем переменной", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		env["EMBEDDING_TIMEOUT_MS"] = "ноль"
		_, err := LoadConfig(envOf(env))
		if err == nil || !strings.Contains(err.Error(), "EMBEDDING_TIMEOUT_MS") {
			t.Fatalf("ожидаю ошибку с именем переменной, получено: %v", err)
		}
	})
}

// TestLoadConfigDBMaxConns — 18.5: DB_MAX_CONNS парсится positiveInt-стилем —
// дефолт 8, явное значение, невалидное — явная ошибка старта с именем
// переменной.
func TestLoadConfigDBMaxConns(t *testing.T) {
	t.Run("не задан — дефолт 8", func(t *testing.T) {
		cfg, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x"}))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if cfg.DBMaxConns != 8 {
			t.Errorf("DBMaxConns = %d, хочу дефолт 8", cfg.DBMaxConns)
		}
	})

	t.Run("явное значение", func(t *testing.T) {
		cfg, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "DB_MAX_CONNS": "16"}))
		if err != nil {
			t.Fatalf("неожиданная ошибка: %v", err)
		}
		if cfg.DBMaxConns != 16 {
			t.Errorf("DBMaxConns = %d, хочу 16", cfg.DBMaxConns)
		}
	})

	for _, raw := range []string{"abc", "0", "-2", "1.5"} {
		_, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "DB_MAX_CONNS": raw}))
		if err == nil || !strings.Contains(err.Error(), "DB_MAX_CONNS") {
			t.Errorf("DB_MAX_CONNS=%q: ожидаю ошибку с именем переменной, получено: %v", raw, err)
		}
	}
}
