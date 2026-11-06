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

// TestToolLimitStopCraneThreshold — с 39.1 первый отказ лимита закрывает
// инструменты (tool_choice=none) и возвращает ошибку с подсказкой «ответь
// текстом»; если модель игнорирует закрытие и снова зовёт инструмент, стоп-кран
// отдаёт финальный результат (Final=true), который executor ago обращает в
// ModelResponse с ResponseForce. Отказанные вызовы в исполнитель не уходят.
func TestToolLimitStopCraneThreshold(t *testing.T) {
	m := NewToolLimitMiddleware(1, nil)
	closed := 0
	m.SetOnExhausted(func() { closed++ })
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
	// Первый отказ: инструменты закрываются, модель видит подсказку.
	te, ok := call("c2").(*events.ToolErrorEvent)
	if !ok {
		t.Fatalf("первый отказ не ошибка инструмента")
	}
	if te.ParentID != "c2" || !strings.Contains(te.Error, "закрыты") || !strings.Contains(te.Error, "текстом") {
		t.Fatalf("отказ не говорит о закрытии инструментов: %+v", te)
	}
	if closed != 1 {
		t.Fatalf("первый отказ не закрыл инструменты: %d", closed)
	}
	if executed != 1 {
		t.Fatalf("отказанный вызов ушёл в исполнитель: %d", executed)
	}

	// Модель игнорирует tool_choice=none — кран: финальный результат.
	tr, ok := call("c3").(*events.ToolResultEvent)
	if !ok {
		t.Fatalf("игнорирование закрытия не включило стоп-кран")
	}
	if !tr.Result.Final {
		t.Fatalf("результат стоп-крана не финальный")
	}
	if tr.ParentID != "c3" {
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

// TestToolLimitStopCraneDirectReacts — на прямом ходе стоп-кран (модель
// игнорирует закрытие инструментов) отдаёт decision=react с «🤔»: финальный
// вызов без инструментов не дал текста, а молчание выглядело бы потерей
// вопроса (39.1).
func TestToolLimitStopCraneDirectReacts(t *testing.T) {
	m := NewToolLimitMiddleware(1, nil)
	m.SetDirect(true)
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

	call("c1") // в лимите
	call("c2") // первый отказ — закрытие инструментов
	ev := call("c3")
	tr, ok := ev.(*events.ToolResultEvent)
	if !ok || !tr.Result.Final {
		t.Fatalf("стоп-кран не сработал на прямом ходе: %T", ev)
	}
	part, ok := events.AsValue(tr.Result.Parts[0]).(events.TextInput)
	if !ok {
		t.Fatalf("part стоп-крана не текст: %T", tr.Result.Parts[0])
	}
	res, err := ParseTurnResult([]byte(part.Content))
	if err != nil {
		t.Fatalf("тело стоп-крана не прошло разбор: %v (%s)", err, part.Content)
	}
	if res.Decision != DecisionReact || res.Reaction != "🤔" {
		t.Fatalf("стоп-кран на прямом ходе = %+v, хочу react/🤔", res)
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
