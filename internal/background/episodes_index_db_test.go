//go:build db

// 17.14: частичный индекс выборки необработанных событий. До фикта такт
// планировщика каждые 15–30 с делал полный скан social_events по
// status='processed' AND summarized_at IS NULL — стоимость такта росла с
// историей; частичный индекс держит только живую выборку, его размер
// пропорционален необработанному хвосту, а не таблице.
package background

import (
	"context"
	"strings"
	"testing"
)

// TestPendingEpisodesIndexMigrated — миграция применяется на чистой базе и
// создаёт частичный индекс ровно под предикат выборки pendingEpisodes.
func TestPendingEpisodesIndexMigrated(t *testing.T) {
	pool := newTestPool(t)

	var indexdef string
	err := pool.QueryRow(context.Background(), `
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'social_events' AND indexname = 'social_events_pending_episodes_idx'`,
	).Scan(&indexdef)
	if err != nil {
		t.Fatalf("частичный индекс выборки эпизодов не создан: %v", err)
	}
	for _, want := range []string{
		"ON public.social_events", "(chat_id, occurred_at, id)",
		"status = 'processed'", "summarized_at IS NULL",
	} {
		if !strings.Contains(indexdef, want) {
			t.Errorf("индекс не содержит %q: %s", want, indexdef)
		}
	}
}
