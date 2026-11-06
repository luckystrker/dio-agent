// Тесты этапа 39 (стоп-кран без текста и стабильный префикс):
//
//	39.1 — первый отказ любого вида закрывает инструменты (tool_choice=none),
//	       модель обязана дать текст; реакция «🤔» — только если финальный
//	       вызов без инструментов не дал валидного TurnResult;
//	39.2 — стабильный префикс: одна схема, один набор инструментов, «не нужны»
//	       → tool_choice=none; фолбэк, если провайдер не принимает tool_choice
//	       вместе со strict json_schema; подсказки полей — в панели;
//	39.3 — card_incomplete:stances не включает инструменты памяти.
package character

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/response"
)

// TestPlanConversationToolsStableSet — набор объявленных инструментов зависит
// только от конфигурации; ситуация хода меняет только режим выбора (39.2).
func TestPlanConversationToolsStableSet(t *testing.T) {
	cases := []struct {
		name                             string
		hasMemory, memoryNeeded, webConf bool
		webAvailable                     bool
		wantMemory, wantWeb              bool
		wantChoice                       string
	}{
		{"память и веб нужны", true, true, true, true, true, true, ""},
		{"память не нужна, веб нужен", true, false, true, true, true, true, ""},
		{"память нужна, веб исчерпан", true, true, true, false, true, true, ""},
		{"ничего не нужно", true, false, true, false, true, true, "none"},
		{"веб выключен, память не нужна", true, false, false, false, true, false, "none"},
		{"веб включён, памяти нет, веб исчерпан", false, false, true, false, false, true, "none"},
		{"памяти нет, веб доступен", false, false, true, true, false, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planConversationTools(tc.hasMemory, tc.memoryNeeded, tc.webConf, tc.webAvailable)
			if plan.Memory != tc.wantMemory || plan.Web != tc.wantWeb || plan.Choice != tc.wantChoice {
				t.Fatalf("план = %+v, хочу memory=%v web=%v choice=%q",
					plan, tc.wantMemory, tc.wantWeb, tc.wantChoice)
			}
		})
	}
}

// TestTurnFieldHintsByFeatures — применимость полей сообщается панелью
// (последнее user-сообщение), а не схемой/system (39.2).
func TestTurnFieldHintsByFeatures(t *testing.T) {
	cases := []struct {
		name string
		f    TurnSchemaFeatures
		want []string
		not  []string
	}{
		{"картинки", TurnSchemaFeatures{Images: true}, []string{"image_notes"}, []string{"image_url", "#m<id>"}},
		{"метки истории", TurnSchemaFeatures{HistoryRefs: true}, []string{"reply_to_by_message", "forward"}, []string{"image_notes", "image_url"}},
		{"веб", TurnSchemaFeatures{Web: true}, []string{"image_url"}, []string{"image_notes", "#m<id>"}},
		{"пустой ход", TurnSchemaFeatures{}, nil, []string{"image_notes", "image_url", "#m<id>"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hints := strings.Join(turnFieldHints(tc.f), "\n")
			for _, want := range tc.want {
				if !strings.Contains(hints, want) {
					t.Fatalf("панель не подсказывает %q: %q", want, hints)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(hints, not) {
					t.Fatalf("панель подсказывает неприменимое %q: %q", not, hints)
				}
			}
		})
	}
}

// TestChatClientToolChoiceNoneWithStrictSchema — на проводе tool_choice="none"
// идёт вместе с объявленными инструментами и strict json_schema (39.2).
func TestChatClientToolChoiceNoneWithStrictSchema(t *testing.T) {
	var bodies []map[string]any
	server := captureServer(t, &bodies)

	tool := testTool(t, "memory_search")
	schemas, err := tool.Schemas(nil)
	if err != nil {
		t.Fatalf("схема инструмента: %v", err)
	}
	opts := config.CallOptions{
		Tools: schemas,
		ResponseSchema: response.RawSchema(TurnResultJSONSchema(),
			response.WithRawName("TurnResult")),
	}

	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	cfg.DisableTools()
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}
	if _, err := client.Call(t.Context(), nil, nil, opts); err != nil {
		t.Fatalf("вызов упал: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("запросов %d, хочу 1", len(bodies))
	}
	if choice, ok := bodies[0]["tool_choice"].(string); !ok || choice != "none" {
		t.Fatalf("tool_choice = %v, хочу none", bodies[0]["tool_choice"])
	}
	if _, has := bodies[0]["tools"]; !has {
		t.Fatalf("определения инструментов не ушли: %v", bodies[0])
	}
	if _, has := bodies[0]["response_format"]; !has {
		t.Fatalf("strict-схема TurnResult не ушла: %v", bodies[0])
	}
}

// TestToolChoiceRejectionFallbackKeepsSchema — если провайдер не принимает
// tool_choice вместе со strict json_schema, один повтор идёт без инструментов
// и tool_choice, но СО схемой TurnResult (39.2).
func TestToolChoiceRejectionFallbackKeepsSchema(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if _, has := body["tool_choice"]; has {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"tool_choice is not supported with response_format json_schema"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","model":"test","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)

	tool := testTool(t, "memory_search")
	schemas, err := tool.Schemas(nil)
	if err != nil {
		t.Fatalf("схема инструмента: %v", err)
	}
	opts := config.CallOptions{
		Tools: schemas,
		ResponseSchema: response.RawSchema(TurnResultJSONSchema(),
			response.WithRawName("TurnResult")),
	}
	cfg := NewChatConfig(server.URL, "test-key", "test-model", 100, 0, 0)
	cfg.DisableTools()
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("клиент не создан: %v", err)
	}
	if _, err := client.Call(t.Context(), nil, nil, opts); err != nil {
		t.Fatalf("фолбэк не спас вызов: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("запросов %d, хочу 2 (отказ + фолбэк)", len(bodies))
	}
	if _, has := bodies[1]["tool_choice"]; has {
		t.Fatalf("в фолбэке остался tool_choice: %v", bodies[1])
	}
	if _, has := bodies[1]["tools"]; has {
		t.Fatalf("в фолбэке остались инструменты: %v", bodies[1])
	}
	if _, has := bodies[1]["response_format"]; !has {
		t.Fatalf("фолбэк потерял strict-схему TurnResult: %v", bodies[1])
	}
}

// ignoringClient — модель, которая зовёт инструмент, пока tool_choice не none
// (состояние видно через cfg.ToolsDisabled), и только потом отвечает текстом.
// Ровно сценарий 39.1: отказы модель не «понимает», но финальный вызов получает.
type ignoringClient struct {
	cfg    *ChatConfig
	mu     sync.Mutex
	calls  int
	closed []bool
	tools  []int
	final  string
}

func (c *ignoringClient) Call(_ context.Context, _ []events.Event, _ *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	closedNow := c.cfg.ToolsDisabled()
	c.mu.Lock()
	n := c.calls
	c.calls++
	c.closed = append(c.closed, closedNow)
	c.tools = append(c.tools, len(opts.Tools))
	c.mu.Unlock()

	if closedNow {
		return &events.ModelResponse{Message: &events.ModelMessage{Content: c.final}}, nil
	}
	return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
		ID: fmt.Sprintf("call-%d", n+1), Name: "probe", Arguments: `{}`,
	}}}}, nil
}

// TestStubbornModelGetsFinalCallWithoutTools — модель, игнорирующая отказы,
// всё равно получает финальный вызов с tool_choice=none (инструменты
// объявлены) и отвечает текстом (39.1).
func TestStubbornModelGetsFinalCallWithoutTools(t *testing.T) {
	fake := &ignoringClient{final: `{"decision":"answer","messages":["вот текст"]}`}
	cfg := NewChatConfigWithClient(fake)
	fake.cfg = cfg

	tool := testTool(t, "probe")
	limitMW := NewToolLimitMiddleware(1, nil)
	limitMW.SetOnExhausted(cfg.DisableTools)

	a := agent.New("stubborn", "",
		agent.WithConfig(cfg),
		agent.WithTools(tool),
		agent.WithMiddleware(func() middleware.Middleware { return limitMW }),
	)
	reply, err := a.Ask(context.Background(), "привет")
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	result, err := ParseTurnResult([]byte(reply.Body()))
	if err != nil {
		t.Fatalf("финальный ответ не валиден: %v (%s)", err, reply.Body())
	}
	if result.Decision != DecisionAnswer || len(result.Messages) != 1 || result.Messages[0] != "вот текст" {
		t.Fatalf("итог = %+v, хочу answer/вот текст", result)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.calls != 3 {
		t.Fatalf("модельных вызовов %d, хочу 3 (tool + отказ + финальный без инструментов)", fake.calls)
	}
	if fake.closed[0] || fake.closed[1] || !fake.closed[2] {
		t.Fatalf("tool_choice не закрылся на финальном вызове: %v", fake.closed)
	}
	for i, n := range fake.tools {
		if n == 0 {
			t.Fatalf("вызов %d потерял определения инструментов (39.2): %v", i, fake.tools)
		}
	}
}

// TestStopCraneIgnoringNoneReacts — модель игнорирует и tool_choice=none:
// стоп-кран замыкает ход; на прямом ходе — react «🤔» с причиной «стоп-кран».
func TestStopCraneIgnoringNoneReacts(t *testing.T) {
	fake := &alwaysToolClient{}
	cfg := NewChatConfigWithClient(fake)

	tool := testTool(t, "probe")
	limitMW := NewToolLimitMiddleware(1, nil)
	limitMW.SetOnExhausted(cfg.DisableTools)
	limitMW.SetDirect(true)

	a := agent.New("crane", "",
		agent.WithConfig(cfg),
		agent.WithTools(tool),
		agent.WithMiddleware(func() middleware.Middleware { return limitMW }),
	)
	reply, err := a.Ask(context.Background(), "привет")
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	result, err := ParseTurnResult([]byte(reply.Body()))
	if err != nil {
		t.Fatalf("тело стоп-крана не валидно: %v (%s)", err, reply.Body())
	}
	if result.Decision != DecisionReact || result.Reaction != "🤔" {
		t.Fatalf("стоп-кран = %+v, хочу react/🤔", result)
	}
	if !strings.Contains(result.Reason, "стоп-кран") {
		t.Fatalf("причина крана не названа: %q", result.Reason)
	}
}

// alwaysToolClient — модель, которая всегда зовёт инструмент (игнорирует и
// tool_choice=none): проверяет резервный путь стоп-крана.
type alwaysToolClient struct{}

func (alwaysToolClient) Call(_ context.Context, _ []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
		ID: "call", Name: "probe", Arguments: `{}`,
	}}}}, nil
}

// TestToolLimitClosedAtStartWarnsThenCrane — инструменты закрыты харнессом с
// самого начала (tool_choice=none, 39.2): первый вызов инструмента получает
// подсказку, повторный — стоп-кран. Иначе упорная модель исполнила бы
// инструмент, который на ходе «не нужен».
func TestToolLimitClosedAtStartWarnsThenCrane(t *testing.T) {
	m := NewToolLimitMiddleware(5, nil)
	m.SetClosed()
	executed := 0
	next := stubToolExec(&executed)
	cx := agocontext.New(nil)

	ev, err := m.OnToolExecution(next, &events.ToolCallEvent{ID: "c1", Name: "memory_search"}, cx)
	if err != nil {
		t.Fatalf("первый вызов: %v", err)
	}
	te, ok := ev.(*events.ToolErrorEvent)
	if !ok || !strings.Contains(te.Error, "закрыты") {
		t.Fatalf("первый вызов при закрытых инструментах не получил подсказку: %#v", ev)
	}
	ev, err = m.OnToolExecution(next, &events.ToolCallEvent{ID: "c2", Name: "memory_search"}, cx)
	if err != nil {
		t.Fatalf("второй вызов: %v", err)
	}
	tr, ok := ev.(*events.ToolResultEvent)
	if !ok || !tr.Result.Final {
		t.Fatalf("повторный вызов не замкнул ход краном: %#v", ev)
	}
	if executed != 0 {
		t.Fatalf("закрытые инструменты всё же исполнились: %d", executed)
	}
}

// TestCraneFallbackResult — резервное тело крана валидно: прямой ход — react
// «🤔», фон — ignore.
func TestCraneFallbackResult(t *testing.T) {
	direct := craneFallbackResult(true)
	if direct.Decision != DecisionReact || direct.Reaction != "🤔" {
		t.Fatalf("прямой кран = %+v", direct)
	}
	if err := direct.Validate(); err != nil {
		t.Fatalf("прямой кран не валиден: %v", err)
	}
	bg := craneFallbackResult(false)
	if bg.Decision != DecisionIgnore {
		t.Fatalf("фоновый кран = %+v, хочу ignore", bg)
	}
	if err := bg.Validate(); err != nil {
		t.Fatalf("фоновый кран не валиден: %v", err)
	}
}

// TestPlanMemoryToolsStancesOnlyNoTools — 39.3: неполнота карточки только из-за
// stances инструменты памяти не включает (отношения — не пропущенный факт).
func TestPlanMemoryToolsStancesOnlyNoTools(t *testing.T) {
	in := Input{Direct: true, Attempt: 1}
	cases := []struct {
		name       string
		bctx       BaseContext
		wantEnable bool
	}{
		{"stances", BaseContext{MemoryCardFitted: false, MemoryCardIncomplete: "stances"}, false},
		{"budget", BaseContext{MemoryCardFitted: false, MemoryCardIncomplete: "budget"}, true},
		{"fts_cut", BaseContext{MemoryCardFitted: false, MemoryCardIncomplete: "fts_cut"}, true},
		{"stances и limit", BaseContext{MemoryCardFitted: false, MemoryCardIncomplete: "stances, limit"}, true},
		{"история урезана", BaseContext{HistoryTrimmed: true, MemoryCardFitted: true}, true},
		{"всё влезло", BaseContext{MemoryCardFitted: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planMemoryTools(in, tc.bctx, true, true, 8)
			if plan.Enabled != tc.wantEnable {
				t.Fatalf("Enabled = %v, хочу %v (%+v)", plan.Enabled, tc.wantEnable, tc.bctx)
			}
		})
	}
}
