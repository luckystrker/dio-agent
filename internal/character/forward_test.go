// Тесты пересылки (этап 37.3): поле forward — метка сообщения этого чата,
// одна пересылка за ход, при ignore/defer мягко обнуляется.
package character

import "testing"

func TestParseTurnResultForward(t *testing.T) {
	ok, err := ParseTurnResult([]byte(`{"decision":"answer","messages":["вот, напомню"],"forward":"#m42"}`))
	if err != nil {
		t.Fatalf("валидный forward отклонён: %v", err)
	}
	if ok.Forward != "#m42" {
		t.Fatalf("forward = %q", ok.Forward)
	}

	// Пересылка — действие, как стикер: при ignore/defer её нет.
	for _, raw := range []string{
		`{"decision":"ignore","forward":"#m42"}`,
		`{"decision":"defer","defer_hours":1,"forward":"#m42"}`,
	} {
		parsed, err := ParseTurnResult([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if parsed.Forward != "" {
			t.Fatalf("%s: forward не обнулён: %q", raw, parsed.Forward)
		}
	}
	// react может переслать (действие без текста).
	parsed, err := ParseTurnResult([]byte(`{"decision":"react","reaction":"🔥","forward":"#m42"}`))
	if err != nil {
		t.Fatalf("react с forward отклонён: %v", err)
	}
	if parsed.Forward != "#m42" {
		t.Fatalf("react: forward = %q", parsed.Forward)
	}
}
