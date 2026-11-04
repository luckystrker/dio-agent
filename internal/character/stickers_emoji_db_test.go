//go:build db

// БД-тесты этапа 33.1: стикер по эмодзи коллекции. Если эмодзи совпадает у
// нескольких стикеров, выбирается тот, что дольше всех не отправлялся.
package character

import (
	"context"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

func TestStickerByEmojiLeastRecentlySent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewStickerRepo()

	first, created, err := repo.Save(ctx, pool, telegram.StickerInfo{
		FileID: "E-A", FileUniqueID: "EU-A", Emoji: "😎",
	}, "первый")
	if err != nil || !created {
		t.Fatalf("первый стикер: id=%d created=%v err=%v", first, created, err)
	}
	second, created, err := repo.Save(ctx, pool, telegram.StickerInfo{
		FileID: "E-B", FileUniqueID: "EU-B", Emoji: "😎",
	}, "второй")
	if err != nil || !created {
		t.Fatalf("второй стикер: id=%d created=%v err=%v", second, created, err)
	}
	now := time.Now()
	if _, err := pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, created_at)
		VALUES (990001, 0, $1, jsonb_build_object('kind','sticker','file_id',$2::text), 'sent', $3)`,
		testChatID, "E-A", now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("старая отправка не вставлена: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, created_at)
		VALUES (990002, 0, $1, jsonb_build_object('kind','sticker','file_id',$2::text), 'sent', $3)`,
		testChatID, "E-B", now.Add(-time.Hour)); err != nil {
		t.Fatalf("недавняя отправка не вставлена: %v", err)
	}

	// Оба отправлялись: выбирается тот, чья отправка была раньше.
	sticker, err := repo.ByEmoji(ctx, pool, "😎")
	if err != nil || sticker == nil || sticker.ID != first {
		t.Fatalf("ByEmoji: %+v err=%v, хочу стикер %d (дольше всех не отправлялся)", sticker, err, first)
	}
	// И через ByLabel по эмодзи — тот же путь.
	sticker, err = repo.ByLabel(ctx, pool, "😎")
	if err != nil || sticker == nil || sticker.ID != first {
		t.Fatalf("ByLabel по эмодзи: %+v err=%v, хочу стикер %d", sticker, err, first)
	}

	// Теперь первый отправлен позже — выбирается второй.
	if _, err := pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, created_at)
		VALUES (990003, 0, $1, jsonb_build_object('kind','sticker','file_id',$2::text), 'sent', $3)`,
		testChatID, "E-A", now); err != nil {
		t.Fatalf("новая отправка не вставлена: %v", err)
	}
	sticker, err = repo.ByEmoji(ctx, pool, "😎")
	if err != nil || sticker == nil || sticker.ID != second {
		t.Fatalf("после новой отправки: %+v err=%v, хочу стикер %d", sticker, err, second)
	}

	// Чужой эмодзи — не ошибка, стикера нет.
	missing, err := repo.ByEmoji(ctx, pool, "🔥")
	if err != nil || missing != nil {
		t.Fatalf("чужой эмодзи: %+v err=%v", missing, err)
	}
	// Служебное заполнение — тоже не стикер.
	missing, err = repo.ByLabel(ctx, pool, "не используется в этом ходу")
	if err != nil || missing != nil {
		t.Fatalf("служебная метка: %+v err=%v", missing, err)
	}
}
