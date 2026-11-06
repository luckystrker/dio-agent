// Модельный слой хода (§17): OpenAI-совместимый провайдер поверх общего
// движка ago (config.RunChatCompat) с одной особенностью хода — forced
// tool-calling на прямом обращении (§10.4, §5.2 приоритет 1): первый вызов
// модели ХОДА обязан вызвать инструмент memory_search. Провайдер ago
// типизированного слота tool_choice не имеет, поэтому ход собирает параметры
// запроса сам: CompatParams + Extra с tool_choice на первом вызове.
//
// Счётчик вызовов живёт на конфиге хода, а не на клиенте: ago создаёт клиента
// заново на каждый запуск (open() → cfg.Create(), third_party/ago/agent/run.go),
// и repair-продолжение §11.3 через reply.Ask — это новый запуск на том же
// конфиге. Общий счётчик гарантирует «на первом вызове прямого хода» ровно
// один раз за ход: repair после первого вызова tool_choice не получает и
// может ответить финальным TurnResult.
//
// Владение контекстом не меняется (§17): всё, что видит модель, собирает
// приложение; провайдер не подключает ничего от себя.
package character

import (
	"context"
	"fmt"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// Короткий повтор 429 внутри вызова (этап 38.5). Живой прогон 2026-11-06:
// провайдер трижды ответил «retry shortly (after 5 s)», транспорт ago с
// MODEL_MAX_RETRIES=0 не повторяет, ход уходил в очередь с паузой 30 с —
// ответ приходил через минуты. Пауза N из подсказки провайдера укладывается в
// ход, если она короткая; более длинную и отсутствие подсказки по-прежнему
// разбирает очередь (§12.1 retry_wait). Повтор живёт в обёртке клиента, общей
// для хода и фона, и не второй раз резервирует бюджет: middleware §16 видит
// один модельный вызов и один operation_id.
const (
	// modelRetryAttempts — сколько коротких повторов делает сам вызов (1–2).
	modelRetryAttempts = 2
	// modelRetryMaxWait — потолок паузы, которую вызов ждёт на месте.
	modelRetryMaxWait = 10 * time.Second
)

// modelRetrySleep — пауза повтора (переменная ради тестов: юнит-тесты не спят).
var modelRetrySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var (
	// modelStatus429Re — отказ провайдера по коду из текста ошибки ago
	// («config: chat completions returned 429: …»): типизированных ошибок
	// провайдер не отдаёт.
	modelStatus429Re = regexp.MustCompile(`returned 429\b`)
	// modelRetryAfterRe — подсказка паузы в теле: «retry shortly (after 5 s)»,
	// «retry after 3 s», «Retry-After: 7» (значение заголовка в секундах, без
	// единицы). Заголовок Retry-After сюда не попадает — его разбирает
	// транспорт ago, когда ретраи включены.
	modelRetryAfterRe = regexp.MustCompile(`(?i)(?:retry[ _-]?after\s*[:=]?\s*(\d+(?:[.,]\d+)?)(?:\s*(?:s\b|sec|second))?|after\s+(\d+(?:[.,]\d+)?)\s*(?:s\b|sec|second))`)
)

// modelRetryAfter — короткая пауза 429 из текста ошибки: 0 < N ≤ 10 с.
// Не 429, нет подсказки или пауза длиннее — повтор не нужен (false).
func modelRetryAfter(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	msg := err.Error()
	if !modelStatus429Re.MatchString(msg) {
		return 0, false
	}
	m := modelRetryAfterRe.FindStringSubmatch(msg)
	if len(m) != 3 {
		return 0, false
	}
	raw := m[1]
	if raw == "" {
		raw = m[2]
	}
	secs, perr := strconv.ParseFloat(strings.ReplaceAll(raw, ",", "."), 64)
	if perr != nil || math.IsNaN(secs) || secs <= 0 {
		return 0, false
	}
	d := time.Duration(secs * float64(time.Second))
	if d > modelRetryMaxWait {
		return 0, false
	}
	return d, true
}

// modelStatusRe — HTTP-код из текста ошибки OpenAI-совместимого слоя ago
// («config: chat completions returned 400: …»): типизированных ошибок нет.
var modelStatusRe = regexp.MustCompile(`returned (\d{3})\b`)

// modelStatus достаёт HTTP-код отказа провайдера из текста ошибки.
func modelStatus(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	m := modelStatusRe.FindStringSubmatch(err.Error())
	if len(m) != 2 {
		return 0, false
	}
	code, perr := strconv.Atoi(m[1])
	if perr != nil {
		return 0, false
	}
	return code, true
}

// isUnsupportedFeatureError — 400, которым провайдер отказывает в ОПЦИОНАЛЬНОЙ
// части запроса («this feature is not yet supported by atlas», живой прогон
// 2026-11-06): тот же запрос повторять бессмысленно, но без схемы/инструментов
// он может пройти.
func isUnsupportedFeatureError(err error) bool {
	code, ok := modelStatus(err)
	if !ok || code != 400 {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"not yet supported", "not supported", "unsupported"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// IsPermanentProviderError — отказ провайдера, который не лечится повтором
// с тем же контекстом: 4xx, кроме 429 (флуд-контроль). Фон использует это,
// чтобы 400 не жёг пять попыток одинаковым запросом (§12.1/§16, этап 38.6).
func IsPermanentProviderError(err error) bool {
	code, ok := modelStatus(err)
	if !ok {
		return false
	}
	return code >= 400 && code < 500 && code != 429
}

// ChatConfig — модельный конфиг одного хода (config.ModelConfig). Один конфиг
// на ход: все запуски агента внутри хода (включая repair §11.3) делят счётчик
// вызовов, поэтому forced tool_choice встаёт только на самый первый вызов.
type ChatConfig struct {
	params config.CompatParams
	// inner — готовый клиент (тесты); Create оборачивает его chatClient'ом,
	// чтобы очистка opts.Tools при исчерпанном лимите была видна и здесь.
	inner config.LLMClient
	// ForcedTool — имя инструмента, чей вызов провайдер требует первым
	// (tool_choice). Пусто — обычный режим, модель решает сама.
	ForcedTool string
	// SessionID — стабильный идентификатор разговора для провайдера
	// (x-opencode-session, OpenCode Go: маршрутизация и кэш промптов).
	// Пусто — заголовок не ставится.
	SessionID string
	// Logf — журнал коротких повторов 429 (nil — стандартный лог процесса).
	Logf func(format string, args ...any)

	mu    sync.Mutex
	calls int
	// toolsOff — лимит обращений к инструментам исчерпан (bug 2026-11-02):
	// следующий модельный вызов уходит без tools и без forced tool_choice,
	// чтобы модель выдала финальный TurnResult. Флаг ставит middleware хода
	// через колбэк; atomic — модельные вызовы и middleware живут в разных
	// горутинах агентского цикла.
	toolsOff atomic.Bool
}

// compile-time проверка контрактов провайдера.
var (
	_ config.ModelConfig = (*ChatConfig)(nil)
	_ config.LLMClient   = (*chatClient)(nil)
)

// NewChatConfig строит конфиг из настроек модели приложения.
func NewChatConfig(endpoint, apiKey, model string, maxOutputTokens int64, timeoutMs, maxRetries int) *ChatConfig {
	return &ChatConfig{
		params: config.CompatParams{
			APIKey:     apiKey,
			BaseURL:    endpoint,
			Model:      model,
			Provider:   config.ProviderOpenAI,
			MaxTokens:  &maxOutputTokens,
			Timeout:    msDuration(timeoutMs),
			MaxRetries: maxRetries,
		},
	}
}

// NewChatConfigWithClient оборачивает готовый LLMClient (скриптовый дубль
// agotesting в тестах, чужой клиент в экспериментах). Create всё равно отдаёт
// chatClient-обёртку: флаг «инструменты отключены» должен наблюдаться и на
// тестовых клиентах (opts.Tools очищается до делегирования), а не только на
// реальном проводе. Скриптовый дубль провода не видит, поэтому механика
// tool_choice проверяется тестом chatClient напрямую.
func NewChatConfigWithClient(client config.LLMClient) *ChatConfig {
	return &ChatConfig{inner: client}
}

// Provider возвращает провайдера (ModelConfig).
func (c *ChatConfig) Provider() config.Provider { return config.ProviderOpenAI }

// Model возвращает имя модели (ModelConfig).
func (c *ChatConfig) Model() string { return c.params.Model }

// Create отдаёт клиент запуска (ModelConfig). Клиентов может быть несколько
// на ход (ago создаёт клиента на каждый запуск, включая repair) — все они
// консультируют один счётчик конфига.
func (c *ChatConfig) Create() (config.LLMClient, error) {
	logf := c.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &chatClient{cfg: c, params: c.params, inner: c.inner, logf: logf}, nil
}

// DisableTools — харнесс исчерпал лимит обращений к инструментам за ход:
// следующие модельные вызовы идут без инструментов (потокобезопасно).
func (c *ChatConfig) DisableTools() { c.toolsOff.Store(true) }

// ToolsDisabled — отключены ли инструменты на этом конфиге хода.
func (c *ChatConfig) ToolsDisabled() bool { return c.toolsOff.Load() }

// nextCall отмечает очередной модельный вызов хода и отвечает, должен ли он
// быть принудительным (tool_choice): только самый первый вызов при заданном
// ForcedTool.
func (c *ChatConfig) nextCall() (force bool, tool string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	force = c.calls == 0 && c.ForcedTool != ""
	tool = c.ForcedTool
	c.calls++
	return force, tool
}

// Clone — контракт ModelConfig; ход клонов не делает, но контракт должен
// выполняться. Клон получает собственный счётчик вызовов (это отдельный ход).
// Поддерживается ключ forced_tool.
func (c *ChatConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	cp := &ChatConfig{params: c.params, inner: c.inner, ForcedTool: c.ForcedTool, SessionID: c.SessionID, Logf: c.Logf}
	for k, v := range overrides {
		switch k {
		case "session_id":
			str, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("character: override session_id: want string, got %T", v)
			}
			cp.SessionID = str
		case "forced_tool":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("character: override forced_tool: want string, got %T", v)
			}
			cp.ForcedTool = s
		default:
			return nil, fmt.Errorf("character: неизвестный ключ клона %q", k)
		}
	}
	return cp, nil
}

// chatClient — клиент одного запуска агента: решение о tool_choice принимает
// общий счётчик конфига хода (см. nextCall), поэтому принуждение срабатывает
// один раз за ход независимо от числа запусков. inner — готовый клиент теста:
// ему передаются те же (возможно, очищенные) CallOptions.
type chatClient struct {
	cfg    *ChatConfig
	params config.CompatParams
	inner  config.LLMClient
	logf   func(format string, args ...any)
}

// Call выполняет один модельный вызов (LLMClient).
func (c *chatClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	// Лимит инструментов исчерпан: провайдер-независимо убираем схемы и НЕ
	// ставим forced tool_choice (bug 2026-11-02) — модель обязана выдать
	// финальный TurnResult, а не звать инструменты дальше.
	toolsOff := c.cfg.ToolsDisabled()
	if toolsOff {
		opts.Tools = nil
	}
	if c.inner != nil {
		return c.inner.Call(ctx, messages, conv, opts)
	}

	params := c.params
	if !toolsOff {
		if force, tool := c.cfg.nextCall(); force {
			params.Extra = map[string]any{
				"tool_choice": map[string]any{
					"type":     "function",
					"function": map[string]any{"name": tool},
				},
			}
		}
	}
	if c.cfg.SessionID != "" {
		if params.ExtraHeaders == nil {
			params.ExtraHeaders = map[string]string{}
		}
		params.ExtraHeaders["x-opencode-session"] = c.cfg.SessionID
		params.ExtraHeaders["User-Agent"] = "dio-agent/0.1 (living telegram character)"
	}
	resp, err := callWithShort429Retry(ctx, params, messages, conv, opts, c.logf)
	if err != nil && isUnsupportedFeatureError(err) {
		// 400 «feature is not yet supported» — опциональная часть запроса не
		// поддержана провайдером (этап 38.6). Один повтор в упрощённом виде:
		// без строгой JSON-схемы и инструментов, причина — в лог. Упрощение
		// картинок не трогает: у отказа по картинке свой честный откат
		// (turn.go, 25.1), который обязан видеть исходную ошибку.
		if c.logf != nil {
			c.logf("провайдер отклонил опциональную часть запроса (400 not supported) — один повтор без строгой схемы и инструментов: %v", err)
		}
		simple := opts
		simple.Tools = nil
		simple.ResponseSchema = nil
		return callWithShort429Retry(ctx, params, messages, conv, simple, c.logf)
	}
	return resp, err
}

// callWithShort429Retry — один логический модельный вызов с 1–2 короткими
// повторами 429 (этап 38.5). Повтор внутри вызова, а не возврат в очередь:
// резерв бюджета §16 уже стоит за этот вызов (middleware резервирует до
// next()), повтор с тем же operation_id не должен расходовать день дважды —
// и не расходует, потому что второго прохода через middleware нет.
func callWithShort429Retry(ctx context.Context, params config.CompatParams, messages []events.Event,
	conv *agocontext.Context, opts config.CallOptions, logf func(string, ...any)) (*events.ModelResponse, error) {
	resp, err := config.RunChatCompat(ctx, params, messages, conv, opts)
	for attempt := 1; err != nil && attempt <= modelRetryAttempts; attempt++ {
		wait, ok := modelRetryAfter(err)
		if !ok {
			break
		}
		if logf != nil {
			logf("провайдер ответил 429 — короткий повтор через %v (попытка %d из %d)", wait, attempt, modelRetryAttempts)
		}
		if serr := modelRetrySleep(ctx, wait); serr != nil {
			// Отмена/остановка во время паузы: ход остаётся в очереди (§12.2),
			// ожидание паузы не продлевается.
			return nil, serr
		}
		resp, err = config.RunChatCompat(ctx, params, messages, conv, opts)
	}
	return resp, err
}

// msDuration переводит миллисекунды конфигурации в time.Duration (0 — без потолка).
func msDuration(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}
