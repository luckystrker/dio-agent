//go:build db

// БД-тест метрики заблокированных чатов (этап 38.7): /stats отдаёт
// deliveries.blocked_chats, /metrics — dio_blocked_chats.
package metrics

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/telegram"
)

func TestSnapshotCountsBlockedChats(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	seedMetricsFixture(t, ctx, pool)

	if _, err := telegram.MarkChatBlocked(ctx, pool, -548919987, "Forbidden: bot was blocked by the user"); err != nil {
		t.Fatalf("чат не помечен: %v", err)
	}

	src := NewPgSource(pool, SourceOptions{
		Now:                    func() time.Time { return time.Now() },
		Baseline:               func(time.Time) character.Mood { return character.Mood{Valence: 0.2, Arousal: 0.5, SocialEnergy: 0.6} },
		Timezone:               time.UTC,
		ConversationCallsLimit: 100,
		BackgroundCallsLimit:   400,
		MaintenanceCallsLimit:  0,
	})
	snap, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatalf("снимок не собран: %v", err)
	}
	if snap.Deliveries.BlockedChats != 1 {
		t.Fatalf("deliveries.blocked_chats = %d, хочу 1", snap.Deliveries.BlockedChats)
	}

	var buf bytes.Buffer
	if err := WritePrometheus(&buf, snap); err != nil {
		t.Fatalf("Prometheus не записан: %v", err)
	}
	if !strings.Contains(buf.String(), "dio_blocked_chats 1") {
		t.Fatalf("в /metrics нет dio_blocked_chats 1:\n%s", buf.String())
	}
}
