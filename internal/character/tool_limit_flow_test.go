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

// toolsSequenceClient — заглушка модели: пока инструменты выданы, всегда зовёт
// инструмент; получив пустой opts.Tools, отвечает финальным JSON. Запоминает
// число схем инструментов на каждом вызове.
type toolsSequenceClient struct {
	mu    sync.Mutex
	calls []int
	final string
}

func (c *toolsSequenceClient) Call(_ context.Context, _ []events.Event, _ *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	c.mu.Lock()
	n := len(c.calls)
	c.calls = append(c.calls, len(opts.Tools))
	c.mu.Unlock()

	if len(opts.Tools) == 0 || n >= 5 {
		return &events.ModelResponse{Message: &events.ModelMessage{Content: c.final}}, nil
	}
	return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
		ID:        fmt.Sprintf("call-%d", n+1),
		Name:      "probe",
		Arguments: `{}`,
	}}}}, nil
}

func (c *toolsSequenceClient) toolCounts() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.calls...)
}

// TestToolLimitDisablesToolsNextModelCall — после первого отказа лимита
// следующий вызов получает opts.Tools пустым, а ответ модели (JSON) становится
// итогом answer. Работает и через NewChatConfigWithClient: обёртка chatClient
// сама чистит инструменты, поэтому тестовая заглушка видит флаг.
func TestToolLimitDisablesToolsNextModelCall(t *testing.T) {
	fake := &toolsSequenceClient{final: `{"decision":"answer","messages":["готово"]}`}
	cfg := NewChatConfigWithClient(fake)

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
	if len(counts) < 3 {
		t.Fatalf("модельных вызовов %d, хочу минимум 3: %v", len(counts), counts)
	}
	if counts[0] == 0 || counts[1] == 0 {
		t.Fatalf("инструменты пропали раньше лимита: %v", counts)
	}
	if counts[2] != 0 {
		t.Fatalf("после отказа лимита инструменты не отключены: %v", counts)
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
	if len(rec.sizes) != 2 || rec.sizes[0] != 1 || rec.sizes[1] != 0 {
		t.Fatalf("размеры opts.Tools = %v, хочу [1 0]", rec.sizes)
	}
}
