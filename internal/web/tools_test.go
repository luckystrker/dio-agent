package web

import (
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agomiddleware "github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

func TestNewToolsetProviderRules(t *testing.T) {
	c := NewClient(0, 0)
	if _, err := NewToolset(c, ProviderOff, "", 0); err != nil {
		t.Fatalf("off без ключа — валидно: %v", err)
	}
	ts, err := NewToolset(c, ProviderOff, "", 0)
	if err != nil {
		t.Fatalf("тулсет off: %v", err)
	}
	if ts.Enabled() {
		t.Fatal("off не должен включать поиск")
	}
	if tools, err := ts.Tools(); err != nil || len(tools) != 0 {
		t.Fatalf("выключенный веб не даёт инструментов: %v %v", tools, err)
	}
	if _, err := NewToolset(c, ProviderTavily, "", 0); err == nil {
		t.Fatal("tavily без ключа должен быть ошибкой")
	}
	if _, err := NewToolset(c, "bogus", "", 0); err == nil {
		t.Fatal("неизвестный провайдер должен быть ошибкой")
	}
	if _, err := NewToolset(c, ProviderDDG, "", 5); err != nil {
		t.Fatalf("ddg без ключа — валидно: %v", err)
	}
}

// TestBudgetMiddlewareGates — лишние вызовы отклоняются ToolError-ом с текстом
// лимита, вызовы другого инструмента не считаются.
func TestBudgetMiddlewareGates(t *testing.T) {
	factories, err := BudgetMiddleware("web_search", 2, 1)
	if err != nil {
		t.Fatalf("бюджет не собран: %v", err)
	}
	if len(factories) != 2 {
		t.Fatalf("фабрик %d, хочу 2 (поиск + fetch)", len(factories))
	}
	mw := factories[0]() // поиск
	cx := agocontext.New(stream.NewMemoryStream())

	call := func(m agomiddleware.Middleware, name string) (string, bool) {
		ev := &events.ToolCallEvent{Name: name}
		var got string
		next := func(e *events.ToolCallEvent, _ *agocontext.Context) (events.Event, error) {
			got = e.Name
			return &events.ToolResultEvent{Name: e.Name}, nil
		}
		out, err := m.OnToolExecution(next, ev, cx)
		if err != nil {
			return "", false
		}
		if te, ok := out.(*events.ToolErrorEvent); ok {
			return te.Error, false
		}
		return got, true
	}

	for range 2 {
		if res, ok := call(mw, "web_search"); !ok || res != "web_search" {
			t.Fatalf("первые два поиска должны проходить: %q ok=%v", res, ok)
		}
	}
	res, ok := call(mw, "web_search")
	if ok || !strings.Contains(res, "лимит исчерпан") {
		t.Fatalf("третий поиск должен гаситься лимитом: %q ok=%v", res, ok)
	}
	if _, ok := call(mw, "web_fetch"); !ok {
		t.Fatal("чужой инструмент не должен считаться бюджетом поиска")
	}
}
