//go:build db

// БД-тесты reasoning модели в журнале хода (26.3): провайдер отдаёт
// reasoning_content, харнесс пишет его в model_output (и отдельно для
// repair-попытки), а при невалидном результате — в turn_skipped. В историю
// модели reasoning не возвращается.
package character

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// reasoningReply — один ответ фейкового провайдера: тело и reasoning.
type reasoningReply struct {
	content   string
	reasoning string
}

// reasoningClient — фейковый провайдер: отдаёт reasoning событием
// ModelReasoning (как config.RunChatCompat после reasoning_content) и
// скриптовое тело ответа.
type reasoningClient struct {
	mu      sync.Mutex
	replies []reasoningReply
	calls   int
}

var _ config.LLMClient = (*reasoningClient)(nil)

func (c *reasoningClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	c.mu.Lock()
	if c.calls >= len(c.replies) {
		c.mu.Unlock()
		return nil, errors.New("скрипт ответов исчерпан")
	}
	reply := c.replies[c.calls]
	c.calls++
	c.mu.Unlock()
	if conv != nil && reply.reasoning != "" {
		if err := conv.Send(&events.ModelReasoning{Content: reply.reasoning}); err != nil {
			return nil, err
		}
	}
	return &events.ModelResponse{Message: &events.ModelMessage{Content: reply.content}}, nil
}

func (f *fixture) withReasoningModel(replies ...reasoningReply) *reasoningClient {
	client := &reasoningClient{replies: replies}
	f.runner.Model = NewChatConfigWithClient(client)
	return client
}

// modelOutputOf — событие model_output хода (nil — нет).
func (f *fixture) modelOutputOf(t *testing.T, ctx context.Context, turnID int64) *ModelOutput {
	t.Helper()
	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	for _, ev := range evs {
		if out, ok := events.AsValue(ev).(ModelOutput); ok {
			return &out
		}
	}
	return nil
}

func (f *fixture) turnSkippedOf(t *testing.T, ctx context.Context, turnID int64) *TurnSkipped {
	t.Helper()
	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	for _, ev := range evs {
		if skip, ok := events.AsValue(ev).(TurnSkipped); ok {
			return &skip
		}
	}
	return nil
}

func TestTurnReasoningInJournalDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(2001)
	f.seedTurn(t, ctx, turnID, "Помнишь пластинки?")
	f.withReasoningModel(reasoningReply{content: validFinalJSON(), reasoning: "проверяю пластинки в памяти"})

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Помнишь пластинки?"}}})
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	if out.Skipped {
		t.Fatalf("ход пропущен: %s", out.SkipReason)
	}

	mo := f.modelOutputOf(t, ctx, turnID)
	if mo == nil {
		t.Fatal("model_output не записан")
	}
	if mo.Reasoning != "проверяю пластинки в памяти" {
		t.Fatalf("reasoning в model_output = %q", mo.Reasoning)
	}
	if mo.ReasoningRepair != "" {
		t.Fatalf("reasoning repair при валидном ответе = %q", mo.ReasoningRepair)
	}

	// Reasoning — только журнал: в поток хода transient-событие не попадает.
	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	for _, ev := range evs {
		if ev.EventType() == "ModelReasoning" {
			t.Fatalf("reasoning попал в durable-журнал отдельным событием: %v", ev)
		}
	}

	// JSON события: пустые поля не пишутся (нет reasoning — нет поля).
	raw, err := json.Marshal(ModelOutput{Result: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("model_output не сериализуется: %v", err)
	}
	if strings.Contains(string(raw), "reasoning") {
		t.Fatalf("пустой reasoning попал в JSON события: %s", raw)
	}
}

func TestTurnReasoningRepairInJournalDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(2002)
	f.seedTurn(t, ctx, turnID, "Как дела?")
	f.withReasoningModel(
		reasoningReply{content: `{"decision":"answer"}`, reasoning: "решил ответить без текста"},
		reasoningReply{content: validFinalJSON(), reasoning: "исправляю по подсказке"},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Как дела?"}}})
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	if out.Skipped {
		t.Fatalf("ход пропущен: %s", out.SkipReason)
	}
	mo := f.modelOutputOf(t, ctx, turnID)
	if mo == nil {
		t.Fatal("model_output не записан")
	}
	if mo.Repairs != 1 {
		t.Fatalf("repairs=%d, хочу 1", mo.Repairs)
	}
	if mo.Reasoning != "решил ответить без текста" {
		t.Fatalf("reasoning первой попытки = %q", mo.Reasoning)
	}
	if mo.ReasoningRepair != "исправляю по подсказке" {
		t.Fatalf("reasoning repair = %q", mo.ReasoningRepair)
	}
}

func TestTurnReasoningInSkippedDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(2003)
	f.seedTurn(t, ctx, turnID, "Ну?")
	f.withReasoningModel(
		reasoningReply{content: `{"decision":"react"}`, reasoning: "первая мысль"},
		reasoningReply{content: `{"decision":"react"}`, reasoning: "вторая мысль"},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Ну?"}}})
	if err != nil {
		t.Fatalf("ход: %v", err)
	}
	if !out.Skipped {
		t.Fatalf("ход не пропущен: %+v", out)
	}
	skip := f.turnSkippedOf(t, ctx, turnID)
	if skip == nil {
		t.Fatal("turn_skipped не записан")
	}
	for _, want := range []string{"первая мысль", "вторая мысль"} {
		if !strings.Contains(skip.Reason, want) {
			t.Fatalf("turn_skipped не содержит reasoning %q:\n%s", want, skip.Reason)
		}
	}
	if f.modelOutputOf(t, ctx, turnID) != nil {
		t.Fatal("при терминальном пропуске записан model_output")
	}
}
