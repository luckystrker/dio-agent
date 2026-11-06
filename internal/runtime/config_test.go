package runtime

import (
	"os"
	"path/filepath"
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
		if m.MaxRetries != 0 || m.MaxOutputTokens != 1000 || m.DailyCallsLimit != 100 || m.HistoryMaxChars != 200_000 {
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
			"MODEL_MAX_RETRIES":       "-1",
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
		if cfg.Model.MaxRetries != 0 {
			t.Errorf("пустое значение должно давать умолчание 0, получено %d", cfg.Model.MaxRetries)
		}
	})

	t.Run("MODEL_MAX_RETRIES=0 — ретраи провайдера выключены", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		env["MODEL_MAX_RETRIES"] = "0"
		cfg, err := LoadConfig(envOf(env))
		if err != nil {
			t.Fatalf("0 должен быть допустим: %v", err)
		}
		if cfg.Model.MaxRetries != 0 {
			t.Errorf("MaxRetries = %d, хочу 0", cfg.Model.MaxRetries)
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

// TestLoadConfigTimezone — часовой пояс меток истории/панели (AGENT_TIMEZONE):
// по умолчанию Europe/Moscow, явная IANA-зона применяется, неизвестная —
// ошибка запуска (молчаливая подмена исказила бы метки истории).
func TestLoadConfigTimezone(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x"}))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if cfg.Timezone == nil || cfg.Timezone.String() != DefaultTimezone {
		t.Fatalf("таймзона по умолчанию = %v, хочу %s", cfg.Timezone, DefaultTimezone)
	}

	cfg, err = LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "AGENT_TIMEZONE": "Asia/Vladivostok"}))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if cfg.Timezone.String() != "Asia/Vladivostok" {
		t.Fatalf("AGENT_TIMEZONE не применилась: %v", cfg.Timezone)
	}

	if _, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "AGENT_TIMEZONE": "Mars/Olympus"})); err == nil ||
		!strings.Contains(err.Error(), "AGENT_TIMEZONE") {
		t.Fatalf("неизвестная зона должна валить запуск с именем переменной, получено: %v", err)
	}
}

// TestLoadConfigMetricsPort — эндпоинт метрик (этап 31): не задан или пуст —
// HTTP выключен (0); явный порт применяется; мусор и диапазон вне 1..65535 —
// явная ошибка запуска, а не тихий дефолт.
func TestLoadConfigMetricsPort(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x"}))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if cfg.MetricsPort != 0 {
		t.Errorf("MetricsPort по умолчанию = %d, хочу 0 (выключен)", cfg.MetricsPort)
	}

	cfg, err = LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "METRICS_PORT": "9090"}))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if cfg.MetricsPort != 9090 {
		t.Errorf("MetricsPort = %d, хочу 9090", cfg.MetricsPort)
	}

	cfg, err = LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "METRICS_PORT": "   "}))
	if err != nil {
		t.Fatalf("пустая строка — выключенный сервер, не ошибка: %v", err)
	}
	if cfg.MetricsPort != 0 {
		t.Errorf("пустой METRICS_PORT = %d, хочу 0", cfg.MetricsPort)
	}

	for _, raw := range []string{"abc", "0", "-1", "1.5", "65536"} {
		_, err := LoadConfig(envOf(map[string]string{"DATABASE_URL": "x", "METRICS_PORT": raw}))
		if err == nil || !strings.Contains(err.Error(), "METRICS_PORT") {
			t.Errorf("METRICS_PORT=%q: ожидаю ошибку с именем переменной, получено: %v", raw, err)
		}
	}
}

// TestLoadConfigMemoryPrefetch — флаг отката среза 2: по умолчанию true
// (карточка памяти вместо инструментов), явный false — старое поведение,
// мусор — ошибка запуска.
func TestLoadConfigMemoryPrefetch(t *testing.T) {
	full := map[string]string{
		"DATABASE_URL":   "x",
		"MODEL_ENDPOINT": "https://api.example.com/v1",
		"MODEL_API_KEY":  "key",
		"MODEL_MODEL":    "gpt-x",
	}
	cfg, err := LoadConfig(envOf(full))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if !cfg.Model.MemoryPrefetch {
		t.Fatalf("MODEL_MEMORY_PREFETCH по умолчанию должна быть true")
	}

	env := map[string]string{"MODEL_MEMORY_PREFETCH": "false"}
	for k, v := range full {
		env[k] = v
	}
	cfg, err = LoadConfig(envOf(env))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if cfg.Model.MemoryPrefetch {
		t.Fatalf("MODEL_MEMORY_PREFETCH=false не применился")
	}

	env["MODEL_MEMORY_PREFETCH"] = "maybe"
	if _, err := LoadConfig(envOf(env)); err == nil || !strings.Contains(err.Error(), "MODEL_MEMORY_PREFETCH") {
		t.Fatalf("мусорная булева переменная должна валить запуск, получено: %v", err)
	}

	// Пустое значение = умолчание (как у числовых переменных): .env.example
	// обещает «Пусто — true».
	env["MODEL_MEMORY_PREFETCH"] = "   "
	cfg, err = LoadConfig(envOf(env))
	if err != nil {
		t.Fatalf("пустое значение не должно валить запуск: %v", err)
	}
	if !cfg.Model.MemoryPrefetch {
		t.Fatalf("пустое MODEL_MEMORY_PREFETCH должно давать true")
	}
}

// TestLoadConfigFFmpegPath — этап 35: FFMPEG_PATH явный, иначе поиск в PATH,
// затем $HOME/.local/bin/ffmpeg. Несуществующий файл — раскадровка выключена
// (пусто), а не ошибка запуска: кружки/гифки живут превью-кадром.
func TestLoadConfigFFmpegPath(t *testing.T) {
	makeExec := func(t *testing.T, dir, name string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("файл: %v", err)
		}
		return path
	}

	t.Run("явный путь", func(t *testing.T) {
		path := makeExec(t, t.TempDir(), "ffmpeg")
		cfg, err := LoadConfig(envOf(map[string]string{
			"DATABASE_URL": "x",
			"FFMPEG_PATH":  path,
			"PATH":         "/nowhere",
		}))
		if err != nil {
			t.Fatalf("конфиг: %v", err)
		}
		if cfg.FFmpegPath != path {
			t.Fatalf("FFmpegPath = %q, хочу %q", cfg.FFmpegPath, path)
		}
	})

	t.Run("явного пути нет — выключено", func(t *testing.T) {
		cfg, err := LoadConfig(envOf(map[string]string{
			"DATABASE_URL": "x",
			"FFMPEG_PATH":  filepath.Join(t.TempDir(), "нет"),
			"PATH":         "/nowhere",
			"HOME":         t.TempDir(),
		}))
		if err != nil {
			t.Fatalf("конфиг: %v", err)
		}
		if cfg.FFmpegPath != "" {
			t.Fatalf("FFmpegPath = %q, хочу пусто", cfg.FFmpegPath)
		}
	})

	t.Run("поиск в PATH", func(t *testing.T) {
		dir := t.TempDir()
		path := makeExec(t, dir, "ffmpeg")
		cfg, err := LoadConfig(envOf(map[string]string{
			"DATABASE_URL": "x",
			"PATH":         "/nowhere:" + dir,
			"HOME":         t.TempDir(),
		}))
		if err != nil {
			t.Fatalf("конфиг: %v", err)
		}
		if cfg.FFmpegPath != path {
			t.Fatalf("FFmpegPath = %q, хочу %q", cfg.FFmpegPath, path)
		}
	})

	t.Run("домашний .local/bin", func(t *testing.T) {
		home := t.TempDir()
		bin := filepath.Join(home, ".local", "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatalf("каталог: %v", err)
		}
		path := makeExec(t, bin, "ffmpeg")
		cfg, err := LoadConfig(envOf(map[string]string{
			"DATABASE_URL": "x",
			"PATH":         "/nowhere",
			"HOME":         home,
		}))
		if err != nil {
			t.Fatalf("конфиг: %v", err)
		}
		if cfg.FFmpegPath != path {
			t.Fatalf("FFmpegPath = %q, хочу %q", cfg.FFmpegPath, path)
		}
	})

	t.Run("нет нигде", func(t *testing.T) {
		cfg, err := LoadConfig(envOf(map[string]string{
			"DATABASE_URL": "x",
			"PATH":         "/nowhere",
			"HOME":         t.TempDir(),
		}))
		if err != nil {
			t.Fatalf("конфиг: %v", err)
		}
		if cfg.FFmpegPath != "" {
			t.Fatalf("FFmpegPath = %q, хочу пусто", cfg.FFmpegPath)
		}
	})
}

// TestLoadConfigModelDebugDumpDir — этап 40.1: MODEL_DEBUG_DUMP_DIR включает
// дамп модельных вызовов; не задана или пуста — выключен (не ошибка запуска).
func TestLoadConfigModelDebugDumpDir(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{
		"DATABASE_URL":         "x",
		"MODEL_DEBUG_DUMP_DIR": " /var/lib/dio/dumps ",
	}))
	if err != nil {
		t.Fatalf("конфиг: %v", err)
	}
	if cfg.ModelDebugDumpDir != "/var/lib/dio/dumps" {
		t.Fatalf("ModelDebugDumpDir = %q", cfg.ModelDebugDumpDir)
	}

	cfg, err = LoadConfig(envOf(map[string]string{
		"DATABASE_URL":         "x",
		"MODEL_DEBUG_DUMP_DIR": "   ",
	}))
	if err != nil {
		t.Fatalf("пустой каталог не должен валить запуск: %v", err)
	}
	if cfg.ModelDebugDumpDir != "" {
		t.Fatalf("пустой MODEL_DEBUG_DUMP_DIR дал %q, хочу пусто", cfg.ModelDebugDumpDir)
	}
}
