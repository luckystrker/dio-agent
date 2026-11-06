// Юнит-тест потолка фоновой задачи (этап 38.8): explore скуки дошёл до 9
// модельных вызовов и 78 705 токенов — потолок 6 вызовов / 40k токенов
// обязан выключать инструменты и завершать задачу тем, что уже найдено.
package background

import (
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// usageResp — ответ модели с заданным расходом токенов.
func usageResp(prompt, completion int64) *events.ModelResponse {
	return &events.ModelResponse{Usage: events.Usage{PromptTokens: &prompt, CompletionTokens: &completion}}
}

func TestUsageCounterCapsCalls(t *testing.T) {
	disabled := 0
	u := &usageCounter{capCalls: 2, onCap: func() { disabled++ }}
	next := func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
		return usageResp(10, 10), nil
	}
	for i := 0; i < 3; i++ {
		if _, err := u.OnLLMCall(next, nil, nil); err != nil {
			t.Fatalf("вызов %d упал: %v", i, err)
		}
	}
	if disabled != 1 {
		t.Fatalf("инструменты выключены %d раз, хочу 1 (потолок срабатывает один раз)", disabled)
	}
	if !u.capped {
		t.Fatal("потолок вызовов не отмечен")
	}
}

func TestUsageCounterCapsTokens(t *testing.T) {
	disabled := 0
	u := &usageCounter{capTokens: exploreMaxTokens, onCap: func() { disabled++ }}
	next := func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
		return usageResp(20000, 15000), nil
	}
	for i := 0; i < 2; i++ {
		if _, err := u.OnLLMCall(next, nil, nil); err != nil {
			t.Fatalf("вызов %d упал: %v", i, err)
		}
	}
	if disabled != 1 {
		t.Fatalf("инструменты выключены %d раз, хочу 1 (40k токенов на втором вызове)", disabled)
	}
}

func TestUsageCounterUnlimitedWithoutCap(t *testing.T) {
	disabled := 0
	u := &usageCounter{onCap: func() { disabled++ }}
	next := func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
		return usageResp(100000, 100000), nil
	}
	for i := 0; i < 3; i++ {
		_, _ = u.OnLLMCall(next, nil, nil)
	}
	if disabled != 0 {
		t.Fatalf("без потолка инструменты выключены %d раз", disabled)
	}
}
