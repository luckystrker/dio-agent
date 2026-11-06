// Тесты этапа 41 (исправления по независимому ревью этапов 33–39):
//
//	B1 — стоп-кран не срабатывает внутри одного блока tool-call'ов: лимит 2 и
//	     четыре поиска одним блоком дают модельный вызов с tool_choice=none и
//	     ТЕКСТ, а не голую 🤔;
//	M4 — closedByReject только для закрытия отказом лимита, не для
//	     harness-закрытия (tool_choice=none с начала хода).
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

// blockToolsClient — модель, которая одним блоком заказывает несколько
// инструментов, пока инструменты открыты, и отвечает текстом, когда закрыты.
// Ровно живой сценарий B1: лимит 2, в блоке 4 вызова.
type blockToolsClient struct {
	cfg   *ChatConfig
	calls int // инструментов в блоке
	mu    sync.Mutex
	// requests — сколько раз модель спросили; toolsClosed — состояние
	// tool_choice на каждом запросе.
	requests    int
	toolsClosed []bool
	blocks      []int // сколько вызовов было в каждом блоке ответа
}

func (c *blockToolsClient) Call(_ context.Context, _ []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	closed := c.cfg.ToolsDisabled()
	c.mu.Lock()
	c.requests++
	c.toolsClosed = append(c.toolsClosed, closed)
	c.mu.Unlock()
	if closed {
		return &events.ModelResponse{Message: &events.ModelMessage{
			Content: `{"decision":"answer","messages":["во сколько встречаемся — в семь?"]}`,
		}}, nil
	}
	calls := make([]events.ToolCallEvent, 0, c.calls)
	for i := 0; i < c.calls; i++ {
		calls = append(calls, events.ToolCallEvent{
			ID: fmt.Sprintf("call-%d", i+1), Name: "probe", Arguments: `{}`,
		})
	}
	c.mu.Lock()
	c.blocks = append(c.blocks, len(calls))
	c.mu.Unlock()
	return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: calls}}, nil
}

// TestStopCraneDoesNotFireInsideOneToolBlock — B1: отказы внутри ОДНОГО блока
// tool-call'ов не взводят кран — модель ещё не видела подсказку. После блока
// идёт финальный вызов с tool_choice=none, и прямой ход заканчивается текстом.
func TestStopCraneDoesNotFireInsideOneToolBlock(t *testing.T) {
	fake := &blockToolsClient{calls: 4}
	cfg := NewChatConfigWithClient(fake)
	fake.cfg = cfg

	tool := testTool(t, "probe")
	limitMW := NewToolLimitMiddleware(2, nil)
	limitMW.SetOnExhausted(cfg.DisableTools)
	limitMW.SetDirect(true)

	a := agent.New("block", "",
		agent.WithConfig(cfg),
		agent.WithTools(tool),
		agent.WithMiddleware(func() middleware.Middleware { return limitMW }),
	)
	reply, err := a.Ask(context.Background(), "во сколько встречаемся?")
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	result, err := ParseTurnResult([]byte(reply.Body()))
	if err != nil {
		t.Fatalf("ответ не валиден: %v (%s)", err, reply.Body())
	}
	if result.Decision != DecisionAnswer {
		t.Fatalf("прямой вопрос закончился %s (%+v), хочу answer с текстом: кран не должен взводиться внутри блока", result.Decision, result)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("сообщений %d, хочу 1: %+v", len(result.Messages), result)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.requests != 2 {
		t.Fatalf("модельных запросов %d, хочу 2 (блок + финальный с tool_choice=none): %v", fake.requests, fake.blocks)
	}
	if fake.toolsClosed[0] || !fake.toolsClosed[1] {
		t.Fatalf("tool_choice по запросам = %v, хочу [false true]", fake.toolsClosed)
	}
}

// TestClosedByRejectOnlyForLimit — M4: закрытие инструментов харнессом
// (tool_choice=none с начала) не делает финальный невалидный ответ «отказом
// лимита»: toolsClosedNow остаётся false, ход — терминальный пропуск, а не 🤔.
func TestClosedByRejectOnlyForLimit(t *testing.T) {
	m := NewToolLimitMiddleware(5, nil)
	m.SetClosed()
	executed := 0
	next := stubToolExec(&executed)
	cx := agocontext.New(nil)

	// Первый вызов при закрытых харнессом инструментах — подсказка.
	if _, err := m.OnToolExecution(next, &events.ToolCallEvent{ID: "c1", Name: "probe"}, cx); err != nil {
		t.Fatalf("первый вызов: %v", err)
	}
	if m.toolsClosedNow() {
		t.Fatal("harness-закрытие помечено как отказ лимита: невалидный финальный ответ станет 🤔 вместо пропуска")
	}
	// Повтор после подсказки (новый модельный вызов) — кран, но и он не «лимит».
	m.OnLLMCall(func(_ []events.Event, _ *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, nil, cx)
	if _, err := m.OnToolExecution(next, &events.ToolCallEvent{ID: "c2", Name: "probe"}, cx); err != nil {
		t.Fatalf("второй вызов: %v", err)
	}
	if m.toolsClosedNow() {
		t.Fatal("стоп-кран по harness-закрытию не должен включать closedByReject")
	}

	// Отказ лимита — вот это closedByReject (лимит 1, второй вызов).
	limit := NewToolLimitMiddleware(1, nil)
	if _, err := limit.OnToolExecution(next, &events.ToolCallEvent{ID: "c3", Name: "probe"}, cx); err != nil {
		t.Fatalf("вызов в лимите: %v", err)
	}
	if _, err := limit.OnToolExecution(next, &events.ToolCallEvent{ID: "c4", Name: "probe"}, cx); err != nil {
		t.Fatalf("вызов сверх лимита: %v", err)
	}
	if !limit.toolsClosedNow() {
		t.Fatal("отказ лимита не отметил closedByReject")
	}
}

// modelTurn — имитация нового модельного вызова (OnLLMCall): только после него
// вызов инструмента означает «модель уже видела подсказку» (B1).
func modelTurn(t *testing.T, m *ToolLimitMiddleware) {
	t.Helper()
	if _, err := m.OnLLMCall(func(_ []events.Event, _ *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, nil, agocontext.New(nil)); err != nil {
		t.Fatalf("модельный вызов не прошёл через middleware: %v", err)
	}
}
