// Тесты контекстного вызова модели (40.1/40.3): счётчик HTTP-вызовов,
// накопитель reasoning и поведение без Call в контексте.
package modebug

import (
	"context"
	"strings"
	"testing"
)

// TestCallNextSeq — нумерация вызовов хода/задачи идёт с 1 и не сбивается
// параллельными запросами (atomic).
func TestCallNextSeq(t *testing.T) {
	c := NewCall("turn", "246")
	if c.Subject() != (Subject{Kind: "turn", ID: "246"}) {
		t.Fatalf("subject = %+v", c.Subject())
	}
	for want := 1; want <= 3; want++ {
		if got := c.NextSeq(); got != want {
			t.Fatalf("NextSeq() = %d, хочу %d", got, want)
		}
	}

	// Параллельные вызовы не выдают один номер дважды.
	par := NewCall("bg", "task")
	seen := make(chan int, 64)
	for i := 0; i < 64; i++ {
		go func() { seen <- par.NextSeq() }()
	}
	got := make(map[int]bool, 64)
	for i := 0; i < 64; i++ {
		n := <-seen
		if got[n] {
			t.Fatalf("номер вызова %d выдан дважды", n)
		}
		got[n] = true
	}
}

// TestCallReasoningCap — провайдер не раздувает память: число частей
// ограничено, порядок частей сохраняется.
func TestCallReasoningCap(t *testing.T) {
	c := NewCall("turn", "1")
	for i := 0; i < 70; i++ {
		c.AddReasoning("x")
	}
	got := c.TakeReasoning()
	if len(got) != 64 {
		t.Fatalf("накоплено %d частей, хочу 64 (потолок)", len(got))
	}
	if c.TakeReasoning() != "" {
		t.Fatal("TakeReasoning не очистил накопитель")
	}

	// Порядок частей — как пришли от провайдера.
	c2 := NewCall("turn", "2")
	c2.AddReasoning("первая")
	c2.AddReasoning("вторая")
	if got := c2.TakeReasoning(); got != "перваявторая" {
		t.Fatalf("порядок reasoning потерян: %q", got)
	}
}

// TestCallReasoningByteCap — байтовый потолок не даёт копить бесконечно
// длинный reasoning, но и не рвёт уже накопленное.
func TestCallReasoningByteCap(t *testing.T) {
	c := NewCall("turn", "1")
	chunk := strings.Repeat("я", 1000) // 2000 байт
	for i := 0; i < 200; i++ {
		c.AddReasoning(chunk)
	}
	got := c.TakeReasoning()
	if len(got) == 0 {
		t.Fatal("reasoning не накоплен")
	}
	if len(got) > maxCallReasoningBytes+len(chunk) {
		t.Fatalf("накоплено %d байт при потолке %d", len(got), maxCallReasoningBytes)
	}
}

// TestCallEmptyAndNil — пустые значения безопасны: nil-вызов, пустой
// reasoning, nil-ctx.
func TestCallEmptyAndNil(t *testing.T) {
	var c *Call
	if c.Subject() != (Subject{}) {
		t.Fatal("nil Subject не пуст")
	}
	if c.NextSeq() != 0 || c.TakeReasoning() != "" {
		t.Fatal("nil Call ведёт себя не как выключенный")
	}
	c.AddReasoning("x") // не паникует

	c2 := NewCall("turn", "1")
	c2.AddReasoning("")
	if got := c2.TakeReasoning(); got != "" {
		t.Fatalf("пустой reasoning накоплен: %q", got)
	}
}

// TestCallContext — Call едет в context; nil не кладётся и не ломает цепочку.
func TestCallContext(t *testing.T) {
	base := context.Background()
	if got := CallFromContext(base); got != nil {
		t.Fatalf("в пустом ctx найден вызов: %+v", got)
	}
	if got := CallFromContext(nil); got != nil {
		t.Fatalf("CallFromContext(nil) = %+v", got)
	}

	c := NewCall("turn", "5")
	ctx := WithCall(base, c)
	if got := CallFromContext(ctx); got != c {
		t.Fatalf("CallFromContext вернул не тот вызов: %+v", got)
	}
	if got := WithCall(base, nil); got != base {
		t.Fatal("WithCall(nil) должен вернуть исходный ctx")
	}
}

// TestParseID — номер хода из строки; нечисловое имя уходит в Task.
func TestParseID(t *testing.T) {
	cases := map[string]int64{
		"246": 246,
		"0":   0,
		"":    0,
		"12a": 0,
		"-3":  0,
	}
	for raw, want := range cases {
		if got := parseID(raw); got != want {
			t.Fatalf("parseID(%q) = %d, хочу %d", raw, got, want)
		}
	}
}
