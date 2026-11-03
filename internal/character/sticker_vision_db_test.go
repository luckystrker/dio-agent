//go:build db

// БД-тест этапа 25.1: провайдер не принимает webp-стикер → ход честно
// откатывается на текст (honest fallback), а не падает; причина — в лог,
// стикер/гифка остаются пометкой в пачке.
package character

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

// imageRejectClient — модельный клиент: первые failures вызовов отказывают
// «картинку не принимаю», затем отдаёт валидный TurnResult. Тела вызовов
// записываются, чтобы увидеть, ушёл ли повтор без картинок.
type imageRejectClient struct {
	mu       sync.Mutex
	failures int
	calls    int
	bodies   [][]events.Event
}

func (c *imageRejectClient) Call(_ context.Context, messages []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.bodies = append(c.bodies, append([]events.Event{}, messages...))
	if c.calls <= c.failures {
		return nil, fmt.Errorf("400 Bad Request: unsupported image format image/webp")
	}
	return &events.ModelResponse{Message: &events.ModelMessage{
		Content: `{"decision":"answer","messages":["вижу стикер"]}`,
	}}, nil
}

// hasImagePart — в теле запроса есть часть-картинка (BinaryInput/UrlInput).
func hasImagePart(body []events.Event) bool {
	for _, ev := range body {
		req, ok := events.AsValue(ev).(events.ModelRequest)
		if !ok {
			continue
		}
		for _, part := range req.Parts {
			switch p := events.AsValue(part).(type) {
			case events.BinaryInput:
				if p.Kind == events.BinaryImage {
					return true
				}
			case events.UrlInput:
				if p.Kind == events.BinaryImage {
					return true
				}
			}
		}
	}
	return false
}

func TestTurnFallsBackToTextWhenImageRejected(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(301)
	// Пачка: статичный стикер (webp) — картинка плюс эмодзи в тексте.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, turn_id, telegram_message_id)
		VALUES (5000301, $1, 'private', 777, 'Коля', 'sticker', '[стикер 😂]',
		        '{"message":{"message_id":9301,"sticker":{"file_id":"STICKER","file_unique_id":"UNIQ1","emoji":"😂"},"from":{"id":777}}}'::jsonb,
		        'buffered', $2, 9301)`, testChatID, turnID); err != nil {
		t.Fatalf("событие-стикер не вставлено: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO turns (id, chat_id, status) VALUES ($1, $2, 'created')`, turnID, testChatID); err != nil {
		t.Fatalf("turn не вставлен: %v", err)
	}

	client := &imageRejectClient{failures: 1}
	f.runner.Model = NewChatConfigWithClient(client)
	fetcher := &fakePhotoFetcher{media: &telegram.PhotoMedia{Mime: "image/webp", Base64: "UklGRg=="}}
	f.runner.Photos = fetcher

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "[стикер 😂]",
			Raw: []byte(`{"message":{"message_id":9301,"sticker":{"file_id":"STICKER","file_unique_id":"UNIQ1","emoji":"😂"}}}`)}}})
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	if !out.Apply.Applied || out.Skipped {
		t.Fatalf("ход не применился после отката: %+v", out)
	}
	if fetcher.calls != 1 {
		t.Fatalf("стикер скачивался %d раз, хочу 1", fetcher.calls)
	}
	if client.calls != 2 {
		t.Fatalf("модельных вызовов %d, хочу 2 (первый с картинкой, повтор без)", client.calls)
	}
	if !hasImagePart(client.bodies[0]) {
		t.Fatal("первый вызов ушёл без картинки: тест не проверяет откат")
	}
	if hasImagePart(client.bodies[1]) {
		t.Fatal("повтор после отказа по картинке снова ушёл с картинкой")
	}
	// Текст события в повторе остался: стикер не потерян, только эмодзи.
	if !strings.Contains(requestText(client.bodies[1]), "стикер") {
		t.Fatalf("повтор без картинки потерял пометку стикера: %s", requestText(client.bodies[1]))
	}
}

// requestText — текст всех частей запросов тела (для проверки, что пачка не
// потерялась в повторе).
func requestText(body []events.Event) string {
	var b strings.Builder
	for _, ev := range body {
		req, ok := events.AsValue(ev).(events.ModelRequest)
		if !ok {
			continue
		}
		for _, part := range req.Parts {
			if text, ok := events.AsValue(part).(events.TextInput); ok {
				b.WriteString(text.Content)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}
