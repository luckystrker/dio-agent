//go:build db

// БД-тест reasoning фоновой задачи (26.3): путь modelrun.go складывает
// reasoning провайдера в turnModelRun, откуда executor пишет его в
// model_output журнала хода.
package background

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/character"
)

type bgReasoningReply struct {
	content   string
	reasoning string
}

type bgReasoningClient struct {
	mu      sync.Mutex
	replies []bgReasoningReply
	calls   int
}

var _ config.LLMClient = (*bgReasoningClient)(nil)

func (c *bgReasoningClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	c.mu.Lock()
	if c.calls >= len(c.replies) {
		c.mu.Unlock()
		return nil, errors.New("скрипт фоновых ответов исчерпан")
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

func TestRunTurnModelCapturesReasoningDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.sched.model = character.NewChatConfigWithClient(&bgReasoningClient{replies: []bgReasoningReply{
		{content: `{"decision":"answer"}`, reasoning: "сначала без текста"},
		{content: `{"decision":"ignore"}`, reasoning: "исправился по подсказке"},
	}})

	run, err := f.sched.runTurnModel(ctx, "test:reasoning:bg", "система", "как дела?",
		nil, bgScope, "", nil)
	if err != nil {
		t.Fatalf("фоновый модельный ход: %v", err)
	}
	if run.Repairs != 1 {
		t.Fatalf("repairs=%d, хочу 1", run.Repairs)
	}
	if run.Reasoning != "сначала без текста" {
		t.Fatalf("reasoning фона = %q", run.Reasoning)
	}
	if run.ReasoningRepair != "исправился по подсказке" {
		t.Fatalf("reasoning repair фона = %q", run.ReasoningRepair)
	}
}
