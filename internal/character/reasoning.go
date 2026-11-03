// Сбор reasoning модели (26.3). Провайдер отдаёт рассуждение потоком событий
// ModelReasoning (reasoning_content в OpenAI-совместимом ответе); ago помечает
// их transient и в durable-историю не пишет. Харнесс подписывается на поток
// на время модельного вызова и складывает дельты отдельно — только для журнала
// хода, обратно в контекст/историю модели reasoning не возвращается.
package character

import (
	"strings"
	"sync"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agomiddleware "github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

// reasoningJournalRunes — потолок reasoning в журнале: разбор хода не должен
// раздувать событие мегабайтами, но мысль обязана быть узнаваемой.
const reasoningJournalRunes = 4000

// ReasoningCapture — middleware одного хода/фоновой задачи: копит reasoning
// всех модельных вызовов между Take (первая попытка, repair). Потокобезопасен:
// подписчик потока может вызываться из goroutine провайдера.
type ReasoningCapture struct {
	agomiddleware.Base

	mu    sync.Mutex
	parts []string
}

// NewReasoningCapture собирает пустой сборщик.
func NewReasoningCapture() *ReasoningCapture { return &ReasoningCapture{} }

// OnLLMCall подписывается на ModelReasoning на время вызова и складывает
// дельты; reasoning соседних вызовов (tool-цикл) склеивается в один Take.
func (c *ReasoningCapture) OnLLMCall(next agomiddleware.LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	if ctx == nil || ctx.Stream == nil {
		return next(evs, ctx)
	}
	var mu sync.Mutex
	var b strings.Builder
	sub := ctx.Stream.Subscribe(func(ev events.Event) error {
		if r, ok := events.AsValue(ev).(events.ModelReasoning); ok {
			mu.Lock()
			b.WriteString(r.Content)
			mu.Unlock()
		}
		return nil
	}, stream.SubscribeOptions{Condition: func(ev events.Event) bool {
		_, ok := events.AsValue(ev).(events.ModelReasoning)
		return ok
	}})
	defer ctx.Stream.Unsubscribe(sub)
	resp, err := next(evs, ctx)

	mu.Lock()
	part := strings.TrimSpace(b.String())
	mu.Unlock()
	if part != "" {
		c.mu.Lock()
		c.parts = append(c.parts, part)
		c.mu.Unlock()
	}
	return resp, err
}

// Take отдаёт накопленный reasoning и очищает сборщик (следующий Take —
// reasoning repair-попытки). Пусто — провайдер reasoning не отдавал.
func (c *ReasoningCapture) Take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.parts) == 0 {
		return ""
	}
	joined := strings.Join(c.parts, "\n\n")
	c.parts = nil
	return truncateReasoning(joined)
}

// truncateReasoning режет reasoning по рунам, не байтам.
func truncateReasoning(s string) string {
	runes := []rune(s)
	if len(runes) <= reasoningJournalRunes {
		return s
	}
	return string(runes[:reasoningJournalRunes-1]) + "…"
}
