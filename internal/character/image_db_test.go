//go:build db

// БД-тест картинки хода (этап 36.2): скачанные байты ложатся в outbox_images
// в той же транзакции применения, что и строка доставки kind=photo, а без
// картинки пузыря нет вовсе.
package character

import (
	"testing"
)

func TestApplyTurnQueuesPhotoDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3601)
	f.seedTurn(t, ctx, turnID, "скинь картинку")

	image := &OutboxImage{
		SourceURL:   "https://cdn.example.com/a.jpg",
		ContentType: "image/jpeg",
		Bytes:       []byte("jpeg-байты"),
	}
	res, err := ApplyTurn(ctx, f.pool, ApplyOptions{
		TurnID:          turnID,
		ChatID:          testChatID,
		ScopeID:         testScopeID,
		Result:          TurnResult{Decision: DecisionAnswer, Messages: []string{"вот"}, ImageURL: image.SourceURL},
		Image:           image,
		PersonaBaseline: f.persona.MoodBaseline,
		Journal:         f.journal,
	})
	if err != nil {
		t.Fatalf("применение: %v", err)
	}
	if !res.Applied {
		t.Fatal("ход не применён")
	}

	var (
		position int
		kind     string
		outboxID int64
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT position, payload->>'kind', id FROM delivery_outbox
		WHERE turn_id = $1 AND payload->>'kind' = 'photo'`, turnID).
		Scan(&position, &kind, &outboxID); err != nil {
		t.Fatalf("пузырь картинки не создан: %v", err)
	}
	// Картинка — отдельным пузырём после сообщений хода.
	if position != 1 {
		t.Fatalf("позиция картинки %d, хочу 1 (после сообщения)", position)
	}
	var contentType string
	var data []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT content_type, bytes FROM outbox_images WHERE outbox_id = $1`, outboxID).
		Scan(&contentType, &data); err != nil {
		t.Fatalf("байты картинки не сохранены: %v", err)
	}
	if contentType != "image/jpeg" || string(data) != "jpeg-байты" {
		t.Fatalf("картинка в базе = %q (%d байт)", contentType, len(data))
	}
}

func TestApplyTurnWithoutImageHasNoPhotoDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3602)
	f.seedTurn(t, ctx, turnID, "просто текст")

	res, err := ApplyTurn(ctx, f.pool, ApplyOptions{
		TurnID:          turnID,
		ChatID:          testChatID,
		ScopeID:         testScopeID,
		Result:          TurnResult{Decision: DecisionAnswer, Messages: []string{"ответ"}},
		PersonaBaseline: f.persona.MoodBaseline,
		Journal:         f.journal,
	})
	if err != nil {
		t.Fatalf("применение: %v", err)
	}
	if !res.Applied {
		t.Fatal("ход не применён")
	}
	var photos int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'photo'`, turnID).
		Scan(&photos); err != nil {
		t.Fatalf("outbox не прочитан: %v", err)
	}
	if photos != 0 {
		t.Fatalf("пузырей картинки %d, хочу 0", photos)
	}
}
