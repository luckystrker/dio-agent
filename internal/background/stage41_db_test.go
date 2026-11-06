//go:build db

// БД-тест этапа 41 (m5): постоянный отказ провайдера (401/403/400 формы)
// завершает фоновое намерение сразу, а не пятью одинаковыми попытками.
package background

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/character"
)

// permanentFailClient — модельный клиент с «мёртвым ключом».
type permanentFailClient struct{ calls int }

func (c *permanentFailClient) Call(context.Context, []events.Event, *agocontext.Context, config.CallOptions) (*events.ModelResponse, error) {
	c.calls++
	return nil, errors.New(`config: chat completions returned 401: {"error":{"message":"invalid api key"}}`)
}

func TestIntentionPermanentProviderErrorFailsOnceDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.seedPrivateChat(t, ctx, "привет", f.now.Add(-6*time.Hour))
	id := f.createIntention(t, ctx, KindReplyLater, "вернуться к разговору", -6, bgChatID)

	client := &permanentFailClient{}
	f.sched.model = character.NewChatConfigWithClient(client)

	claimed := f.claimDirectly(t, ctx)
	if len(claimed) != 1 {
		t.Fatalf("хочу одно claim-нутое намерение: %+v", claimed)
	}
	out, err := f.sched.runIntention(ctx, claimed[0])
	if err != nil {
		t.Fatalf("исполнение намерения: %v", err)
	}
	if out.Outcome != "failed" {
		t.Fatalf("постоянный отказ провайдера не терминален: %+v", out)
	}
	if client.calls != 1 {
		t.Fatalf("модельных вызовов %d, хочу 1 (без пяти одинаковых попыток)", client.calls)
	}
	if st := f.intentionStatus(t, ctx, id); st != "failed" {
		t.Fatalf("намерение в статусе %q, хочу failed", st)
	}
}
