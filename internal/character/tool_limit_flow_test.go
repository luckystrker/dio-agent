// Тесты проводки лимита инструментов (bug 2026-11-02): как только лимит
// исчерпан, следующий модельный вызов уходит без инструментов и без forced
// tool_choice — модель обязана выдать финальный TurnResult, а не молча
// превращать прямой вопрос в ignore.
package character

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
)

// toolsSequenceClient — заглушка модели: пока инструменты не закрыты
// (tool_choice не none), зовёт инструмент; после закрытия отвечает финальным
// JSON. Запоминает число схем инструментов и состояние «закрыто» на каждом
// вызове: с 39.2 определения остаются в запросе, меняется только tool_choice.
type toolsSequenceClient struct {
	cfg    *ChatConfig
	mu     sync.Mutex
	calls  []int
	closed []bool
	final  string
}

func (c *toolsSequenceClient) Call(_ context.Context, _ []events.Event, _ *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	c.mu.Lock()
	closedNow := c.cfg != nil && c.cfg.ToolsDisabled()
	n := len(c.calls)
	c.calls = append(c.calls, len(opts.Tools))
	c.closed = append(c.closed, closedNow)
	c.mu.Unlock()

	if closedNow || len(opts.Tools) == 0 || n >= 5 {
		return &events.ModelResponse{Message: &events.ModelMessage{Content: c.final}}, nil
	}
	return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
		ID:        fmt.Sprintf("call-%d", n+1),
		Name:      "probe",
		Arguments: `{}`,
	}}}}, nil
}

// bindTurnConfig — ход через Runner создаёт свой конфиг на ход; заглушка
// получает именно его (turnConfigBinder), иначе не увидит tool_choice=none.
func (c *toolsSequenceClient) bindTurnConfig(cfg *ChatConfig) {
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
}

func (c *toolsSequenceClient) toolCounts() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.calls...)
}

func (c *toolsSequenceClient) closedFlags() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bool(nil), c.closed...)
}

// TestToolLimitClosesToolsNextModelCall — после первого отказа лимита
// следующий вызов идёт с tool_choice=none (cfg.ToolsDisabled), но определения
// инструментов остаются объявлены (39.2), а ответ модели (JSON) становится
// итогом answer.
func TestToolLimitClosesToolsNextModelCall(t *testing.T) {
	fake := &toolsSequenceClient{final: `{"decision":"answer","messages":["готово"]}`}
	cfg := NewChatConfigWithClient(fake)
	fake.cfg = cfg

	tool := testTool(t, "probe")
	limitMW := NewToolLimitMiddleware(1, nil)
	limitMW.SetOnExhausted(cfg.DisableTools)

	a := agent.New("tool-limit", "",
		agent.WithConfig(cfg),
		agent.WithTools(tool),
		agent.WithMiddleware(func() middleware.Middleware { return limitMW }),
	)
	reply, err := a.Ask(context.Background(), "привет")
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	body := reply.Body()
	result, err := ParseTurnResult([]byte(body))
	if err != nil {
		t.Fatalf("финальный JSON не прошёл разбор: %v (%s)", err, body)
	}
	if result.Decision != DecisionAnswer || len(result.Messages) != 1 || result.Messages[0] != "готово" {
		t.Fatalf("итог = %+v, хочу answer/готово", result)
	}

	counts := fake.toolCounts()
	closed := fake.closedFlags()
	if len(counts) != 3 {
		t.Fatalf("модельных вызовов %d, хочу 3 (tool + отказ + финал): %v", len(counts), counts)
	}
	for i, n := range counts {
		if n == 0 {
			t.Fatalf("вызов %d потерял определения инструментов (39.2): %v", i, counts)
		}
	}
	if closed[0] || closed[1] {
		t.Fatalf("инструменты закрыты раньше отказа: %v", closed)
	}
	if !closed[2] {
		t.Fatalf("после отказа лимита tool_choice не none: %v", closed)
	}
}

// recordingClient — записывает opts.Tools и делегирует заглушке. Нужен, чтобы
// проверить обёртку Create для NewChatConfigWithClient без провода провайдера.
type recordingClient struct {
	inner config.LLMClient
	mu    sync.Mutex
	sizes []int
}

func (c *recordingClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	c.mu.Lock()
	c.sizes = append(c.sizes, len(opts.Tools))
	c.mu.Unlock()
	return c.inner.Call(ctx, messages, conv, opts)
}

func TestChatConfigWithClientObservesDisabledTools(t *testing.T) {
	fake := &toolsSequenceClient{final: `{"decision":"ignore"}`}
	rec := &recordingClient{inner: fake}
	cfg := NewChatConfigWithClient(rec)
	fake.cfg = cfg
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}
	tool := testTool(t, "probe")
	schemas, err := tool.Schemas(nil)
	if err != nil {
		t.Fatalf("схема не получена: %v", err)
	}
	opts := config.CallOptions{Tools: schemas}

	if _, err := client.Call(context.Background(), nil, nil, opts); err != nil {
		t.Fatalf("первый вызов: %v", err)
	}
	cfg.DisableTools()
	if _, err := client.Call(context.Background(), nil, nil, opts); err != nil {
		t.Fatalf("второй вызов: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	// 39.2: определения инструментов не пропадают — меняется только tool_choice.
	if len(rec.sizes) != 2 || rec.sizes[0] != 1 || rec.sizes[1] != 1 {
		t.Fatalf("размеры opts.Tools = %v, хочу [1 1]", rec.sizes)
	}
}

// repairSequenceClient — заглушка модели для 21.7: зовёт инструмент, пока
// схемы выданы; после исчерпания лимита (opts.Tools пуст) отвечает невалидным
// answer без сообщений, затем — валидным. Запоминает размер opts.Tools на
// каждом вызове, чтобы repair-продолжение тоже проверить.
type repairSequenceClient struct {
	mu     sync.Mutex
	counts []int
	call   int
}

func (c *repairSequenceClient) Call(_ context.Context, _ []events.Event, _ *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	c.mu.Lock()
	n := c.call
	c.call++
	c.counts = append(c.counts, len(opts.Tools))
	c.mu.Unlock()

	switch {
	case n < 2:
		return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
			ID:        fmt.Sprintf("call-%d", n+1),
			Name:      "probe",
			Arguments: `{}`,
		}}}}, nil
	case n == 2:
		// Модель без инструментов выдала невалидный ответ: answer без сообщений.
		return &events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":[]}`}}, nil
	default:
		return &events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"react","reaction":"👀"}`}}, nil
	}
}

func (c *repairSequenceClient) toolCounts() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.counts...)
}

// TestRepairCallAfterToolLimitKeepsToolsDeclared — после исчерпания лимита
// repair-продолжение (reply.Ask) идёт с tool_choice=none, но определения
// инструментов остаются в запросе (39.2): модель обязана выдать финальный
// TurnResult, а префикс запроса не меняется.
func TestRepairCallAfterToolLimitKeepsToolsDeclared(t *testing.T) {
	fake := &repairSequenceClient{}
	cfg := NewChatConfigWithClient(fake)

	tool := testTool(t, "probe")
	limitMW := NewToolLimitMiddleware(1, nil)
	limitMW.SetOnExhausted(cfg.DisableTools)

	a := agent.New("tool-limit-repair", "",
		agent.WithConfig(cfg),
		agent.WithTools(tool),
		agent.WithMiddleware(func() middleware.Middleware { return limitMW }),
	)
	reply, err := a.Ask(context.Background(), "привет")
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	if _, err := ParseTurnResult([]byte(reply.Body())); err == nil {
		t.Fatalf("первый ответ должен быть невалидным, получено: %s", reply.Body())
	}
	if !cfg.ToolsDisabled() {
		t.Fatal("после отказа лимита инструменты не закрыты (tool_choice не none)")
	}

	fixed, err := reply.Ask(context.Background(), repairHint(fmt.Errorf("answer требует от 1 до 4 сообщений")))
	if err != nil {
		t.Fatalf("repair-вызов упал: %v", err)
	}
	result, err := ParseTurnResult([]byte(fixed.Body()))
	if err != nil {
		t.Fatalf("repair-ответ не валиден: %v (%s)", err, fixed.Body())
	}
	if result.Decision != DecisionReact || result.Reaction != "👀" {
		t.Fatalf("repair-ответ = %+v, хочу react/👀", result)
	}

	counts := fake.toolCounts()
	if len(counts) < 4 {
		t.Fatalf("модельных вызовов %d, хочу минимум 4 (2 tool + финал + repair)", len(counts))
	}
	for i, n := range counts {
		if n == 0 {
			t.Fatalf("вызов %d потерял определения инструментов (39.2): %v", i, counts)
		}
	}
}
