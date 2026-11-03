//go:build db

// История после правки (30.4): актуальная версия текста исходного сообщения,
// событие «поправил…» в историю как реплика не попадает.
package character

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

func TestRecentHistoryShowsEditedVersion(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	const chatID = 777

	if _, err := telegram.IngestUpdate(ctx, pool,
		[]byte(fmt.Sprintf(`{"update_id":9200,"message":{"message_id":44,"from":{"id":5,"first_name":"Яна"},"chat":{"id":%d,"type":"private"},"text":"привет"}}`, chatID))); err != nil {
		t.Fatalf("исходное: %v", err)
	}
	if _, err := telegram.IngestUpdate(ctx, pool,
		[]byte(fmt.Sprintf(`{"update_id":9201,"edited_message":{"message_id":44,"from":{"id":5,"first_name":"Яна"},"chat":{"id":%d,"type":"private"},"date":%d,"edit_date":%d,"text":"привет, я тут"}}`,
			chatID, time.Now().Unix(), time.Now().Unix()))); err != nil {
		t.Fatalf("правка: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE social_events SET status = 'processed' WHERE chat_id = $1`, chatID); err != nil {
		t.Fatalf("статусы: %v", err)
	}

	history, err := RecentHistory(ctx, pool, chatID, 10, 0)
	if err != nil {
		t.Fatalf("история: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("история: %d реплик, хочу 1 (без дубля правки): %+v", len(history), history)
	}
	if history[0].Content != "привет, я тут" {
		t.Fatalf("история показывает не актуальную версию: %q", history[0].Content)
	}
	if strings.Contains(history[0].Content, "поправил") {
		t.Fatalf("событие правки попало в историю как реплика: %q", history[0].Content)
	}
}
