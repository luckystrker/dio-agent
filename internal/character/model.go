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
	"sync"
	"time"

	"ago/config"
	agocontext "ago/context"
	"ago/events"
)

// ChatConfig — модельный конфиг одного хода (config.ModelConfig). Один конфиг
// на ход: все запуски агента внутри хода (включая repair §11.3) делят счётчик
// вызовов, поэтому forced tool_choice встаёт только на самый первый вызов.
type ChatConfig struct {
	params config.CompatParams
	// inner — готовый клиент (тесты); если задан, Create отдаёт его как есть.
	inner config.LLMClient
	// ForcedTool — имя инструмента, чей вызов провайдер требует первым
	// (tool_choice). Пусто — обычный режим, модель решает сама.
	ForcedTool string

	mu    sync.Mutex
	calls int
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
// agotesting в тестах, чужой клиент в экспериментах). Скриптовый дубль провода
// не видит, поэтому механика tool_choice проверяется тестом chatClient напрямую.
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
	if c.inner != nil {
		return c.inner, nil
	}
	return &chatClient{cfg: c, params: c.params}, nil
}

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
	cp := &ChatConfig{params: c.params, inner: c.inner, ForcedTool: c.ForcedTool}
	for k, v := range overrides {
		switch k {
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
// один раз за ход независимо от числа запусков.
type chatClient struct {
	cfg    *ChatConfig
	params config.CompatParams
}

// Call выполняет один модельный вызов (LLMClient).
func (c *chatClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	params := c.params
	if force, tool := c.cfg.nextCall(); force {
		params.Extra = map[string]any{
			"tool_choice": map[string]any{
				"type":     "function",
				"function": map[string]any{"name": tool},
			},
		}
	}
	return config.RunChatCompat(ctx, params, messages, conv, opts)
}

// msDuration переводит миллисекунды конфигурации в time.Duration (0 — без потолка).
func msDuration(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}
