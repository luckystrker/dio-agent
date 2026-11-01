// Юнит-тесты стоп-крана серии отказов лимита инструментов (16.5) — без БД:
// границы счётчика (второй отказ — ещё ошибка, третий — кран), тело крана —
// валидный TurnResult(ignore) с ровно одним part (контракт finalContent ago),
// лимит 0 не ограничивает вовсе.
package character

import (
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
)

// stubToolExec — фейковый исполнитель: считает вызовы, отвечает обычным
// (не финальным) результатом.
func stubToolExec(calls *int) middleware.ToolExecFunc {
	return func(ev *events.ToolCallEvent, _ *agocontext.Context) (events.Event, error) {
		*calls++
		return &events.ToolResultEvent{ParentID: ev.ID, Name: ev.Name, Result: events.NewToolResult(false, "ok")}, nil
	}
}

// TestToolLimitStopCraneThreshold — приёмка 16.5, границы серии: первый отказ и
// второй — ToolErrorEvent (модель видит отказ и может завершиться сама), третий
// подряд — финальный результат (Final=true), который executor ago обращает в
// ModelResponse с ResponseForce; отказанные вызовы в исполнитель не уходят.
func TestToolLimitStopCraneThreshold(t *testing.T) {
	m := NewToolLimitMiddleware(1, nil)
	executed := 0
	next := stubToolExec(&executed)

	call := func(id string) events.Event {
		t.Helper()
		out, err := m.OnToolExecution(next, &events.ToolCallEvent{ID: id, Name: "memory_search"}, nil)
		if err != nil {
			t.Fatalf("вызов %s: OnToolExecution ошибся: %v", id, err)
		}
		return out
	}

	// В лимите — исполняется настоящим исполнителем.
	if ev := call("c1"); executed != 1 {
		t.Fatalf("вызов в лимите не дошёл до исполнителя: %T", ev)
	}
	// Первый и второй подряд отказ — ошибка инструмента, кран ещё не сработал
	// (преждевременный кран отнимал бы у модели шанс завершить ход ответом).
	for i, id := range []string{"c2", "c3"} {
		te, ok := call(id).(*events.ToolErrorEvent)
		if !ok {
			t.Fatalf("отказ %d (%s) не ошибка инструмента — кран сработал раньше времени", i+1, id)
		}
		if te.ParentID != id || te.Error == "" {
			t.Fatalf("отказ %d не отвечает вызову %s или без текста: %+v", i+1, id, te)
		}
	}
	if executed != 1 {
		t.Fatalf("отказанные вызовы ушли в исполнитель: %d", executed)
	}

	// Третий подряд — кран: финальный результат, отвечающий вызову.
	tr, ok := call("c4").(*events.ToolResultEvent)
	if !ok {
		t.Fatalf("третий подряд отказ не включил стоп-кран")
	}
	if !tr.Result.Final {
		t.Fatalf("результат стоп-крана не финальный")
	}
	if tr.ParentID != "c4" {
		t.Fatalf("стоп-кран не отвечает вызову: parent=%s", tr.ParentID)
	}
	if executed != 1 {
		t.Fatalf("стоп-кран дёрнул исполнитель: %d", executed)
	}

	// Тело крана — валидный TurnResult (§11.2): ход применится как ignore.
	// finalContent ago требует ровно один part, иначе ход упадёт ошибкой.
	if len(tr.Result.Parts) != 1 {
		t.Fatalf("у финального результата %d part, хочу ровно 1 (контракт finalContent ago)", len(tr.Result.Parts))
	}
	part, ok := events.AsValue(tr.Result.Parts[0]).(events.TextInput)
	if !ok {
		t.Fatalf("part стоп-крана не текст: %T", tr.Result.Parts[0])
	}
	res, err := ParseTurnResult([]byte(part.Content))
	if err != nil {
		t.Fatalf("тело стоп-крана не прошло строгий разбор TurnResult: %v (тело: %s)", err, part.Content)
	}
	if res.Decision != DecisionIgnore {
		t.Fatalf("решение стоп-крана %q, хочу ignore", res.Decision)
	}
	if !strings.Contains(res.Reason, "стоп-кран") {
		t.Fatalf("причина стоп-крана не названа: %q", res.Reason)
	}
}

// TestToolLimitZeroDisablesCrane — лимит 0 = не ограничивать (§11.2): сколько бы
// вызовов ни было, все уходят в исполнитель, отказов и крана нет.
func TestToolLimitZeroDisablesCrane(t *testing.T) {
	m := NewToolLimitMiddleware(0, nil)
	executed := 0
	next := stubToolExec(&executed)
	for i := 0; i < stopCraneRejects+3; i++ {
		id := string(rune('a' + i))
		if _, err := m.OnToolExecution(next, &events.ToolCallEvent{ID: id, Name: "memory_search"}, nil); err != nil {
			t.Fatalf("вызов %s: %v", id, err)
		}
	}
	if executed != stopCraneRejects+3 {
		t.Fatalf("лимит 0 ограничивает: исполнено %d из %d", executed, stopCraneRejects+3)
	}
}
