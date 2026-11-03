// Юнит-тесты сбора reasoning модели (26.3): провайдер отдаёт его потоком
// событиями ModelReasoning, которые ago не сохраняет в историю; харнесс
// собирает их для журнала, но не возвращает модели.
package character

import (
	"errors"
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

func TestReasoningCaptureCollectsDeltas(t *testing.T) {
	capture := NewReasoningCapture()
	st := stream.NewMemoryStream()
	cx := agocontext.New(st)

	next := func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		if err := ctx.Send(&events.ModelReasoning{Content: "сначала "}); err != nil {
			return nil, err
		}
		if err := ctx.Send(&events.ModelReasoning{Content: "подумал"}); err != nil {
			return nil, err
		}
		return &events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}}, nil
	}
	if _, err := capture.OnLLMCall(next, nil, cx); err != nil {
		t.Fatalf("модельный вызов: %v", err)
	}
	if got := capture.Take(); got != "сначала подумал" {
		t.Fatalf("reasoning = %q, хочу склейку дельт", got)
	}
	if got := capture.Take(); got != "" {
		t.Fatalf("повторный Take вернул %q, хочу пусто", got)
	}
}

func TestReasoningCaptureTruncates(t *testing.T) {
	capture := NewReasoningCapture()
	st := stream.NewMemoryStream()
	cx := agocontext.New(st)
	long := strings.Repeat("я", reasoningJournalRunes+500)

	next := func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		if err := ctx.Send(&events.ModelReasoning{Content: long}); err != nil {
			return nil, err
		}
		return &events.ModelResponse{}, nil
	}
	if _, err := capture.OnLLMCall(next, nil, cx); err != nil {
		t.Fatalf("модельный вызов: %v", err)
	}
	got := []rune(capture.Take())
	if len(got) > reasoningJournalRunes {
		t.Fatalf("reasoning %d рун, потолок %d", len(got), reasoningJournalRunes)
	}
	if !strings.HasPrefix(string(got), strings.Repeat("я", 10)) {
		t.Fatalf("обрезанный reasoning потерял начало: %q", string(got[:20]))
	}
}

func TestInvalidResultSkipReasonIncludesReasoning(t *testing.T) {
	err := &invalidResultError{
		err:             errors.New("валидация"),
		first:           `{"decision":"answer"}`,
		repair:          `{"decision":"answer"}`,
		firstReasoning:  "первая мысль",
		repairReasoning: "вторая мысль",
	}
	reason := err.skipReason()
	for _, want := range []string{"первая мысль", "вторая мысль", `{"decision":"answer"}`} {
		if !strings.Contains(reason, want) {
			t.Fatalf("причина пропуска не содержит %q:\n%s", want, reason)
		}
	}
}
