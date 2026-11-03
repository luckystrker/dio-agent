//go:build db

// БД-тесты инициатив в группы (30.3): живой разговор с ботом, близость темы к
// истории группы, приватность источника и запрет check_in в группу.
package background

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/memory"
)

func newGroupShareScheduler(t *testing.T) *Scheduler {
	t.Helper()
	pool := newTestPool(t)
	toolset, err := memory.NewToolset(pool, &memory.Searcher{
		Embedder: memory.NewHashEmbedder(),
		Items:    memory.Items{},
	})
	if err != nil {
		t.Fatalf("память: %v", err)
	}
	return &Scheduler{
		pool:       pool,
		intentions: NewIntentionRepo(pool),
		memory:     toolset,
		now:        time.Now,
	}
}

// seedGroupChatHistory наполняет историю группы и лички.
func seedGroupChatHistory(t *testing.T, s *Scheduler, groupText, privateText string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, kind, text, raw_update, status, occurred_at)
		VALUES (9300, -500, 'group', 'message', $1, '{}'::jsonb, 'processed', now() - interval '2 hours')`,
		groupText); err != nil {
		t.Fatalf("история группы: %v", err)
	}
	if privateText != "" {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO social_events (telegram_update_id, chat_id, chat_type, kind, text, raw_update, status, occurred_at)
			VALUES (9301, 500, 'private', 'message', $1, '{}'::jsonb, 'processed', now() - interval '2 hours')`,
			privateText); err != nil {
			t.Fatalf("история лички: %v", err)
		}
	}
	// Бот писал в группу, человек ответил после — живой разговор.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, created_at, updated_at)
		VALUES (9300, 0, -500, '{"kind":"message","text":"ага"}'::jsonb, 'sent', now() - interval '90 minutes', now())`); err != nil {
		t.Fatalf("ответ бота: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, kind, text, raw_update, status, occurred_at)
		VALUES (9302, -500, 'group', 'message', 'ну да', '{}'::jsonb, 'processed', now() - interval '60 minutes')`); err != nil {
		t.Fatalf("ответ человека: %v", err)
	}
}

func createShareIntention(t *testing.T, s *Scheduler, chatID int64, topic string, audience string) Intention {
	t.Helper()
	ctx := context.Background()
	var provenanceID int64
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO provenance (kind, ref, scopes) VALUES ('journal_event', $1, ARRAY[$2]) RETURNING id`,
		fmt.Sprintf("turn:%d", time.Now().UnixNano()), audience).Scan(&provenanceID); err != nil {
		t.Fatalf("provenance: %v", err)
	}
	var id int64
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO intentions (kind, target_chat_id, topic, reason, provenance_id, dedupe_key)
		VALUES ('share', $1, $2, 'проверка', $3, $4) RETURNING id`,
		chatID, topic, provenanceID, fmt.Sprintf("share:%d:%d", chatID, time.Now().UnixNano())).Scan(&id); err != nil {
		t.Fatalf("намерение: %v", err)
	}
	return Intention{ID: id, Kind: KindShare, TargetChatID: chatID, Topic: topic}
}

func TestGroupShareAllowedWhenLiveAndTopicCloser(t *testing.T) {
	s := newGroupShareScheduler(t)
	seedGroupChatHistory(t, s, "старые пластинки и треск", "налоги и отчётность")
	it := createShareIntention(t, s, -500, "старые пластинки и треск", memory.ChatScope("-500"))

	allowed, reason, err := s.groupInitiativeAllowed(context.Background(), it, time.Now())
	if err != nil {
		t.Fatalf("проверка: %v", err)
	}
	if !allowed {
		t.Fatalf("share в живую группу с близкой темой запрещён: %s", reason)
	}
}

func TestGroupShareDeniedWithoutLiveConversation(t *testing.T) {
	s := newGroupShareScheduler(t)
	seedGroupChatHistory(t, s, "старые пластинки и треск", "налоги и отчётность")
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE delivery_outbox SET created_at = now() - interval '7 hours'`); err != nil {
		t.Fatalf("старение ответа бота: %v", err)
	}
	it := createShareIntention(t, s, -500, "старые пластинки и треск", memory.ChatScope("-500"))

	allowed, _, err := s.groupInitiativeAllowed(context.Background(), it, time.Now())
	if err != nil {
		t.Fatalf("проверка: %v", err)
	}
	if allowed {
		t.Fatalf("share без живого разговора за 6 часов разрешён")
	}
}

func TestGroupShareDeniedForPrivateSource(t *testing.T) {
	s := newGroupShareScheduler(t)
	seedGroupChatHistory(t, s, "старые пластинки и треск", "налоги и отчётность")
	// Намерение целится в группу, но основание — приватный скоуп лички.
	it := createShareIntention(t, s, -500, "старые пластинки и треск", memory.ChatScope("500"))

	allowed, reason, err := s.groupInitiativeAllowed(context.Background(), it, time.Now())
	if err != nil {
		t.Fatalf("проверка: %v", err)
	}
	if allowed {
		t.Fatalf("share из приватного источника ушёл бы в группу: %s", reason)
	}
}

func TestGroupShareDeniedWhenTopicCloserToPrivate(t *testing.T) {
	s := newGroupShareScheduler(t)
	// Тема дословно в личке — она ближе личкам (0 < 0 не выполнено).
	seedGroupChatHistory(t, s, "старые пластинки и треск", "старые пластинки и треск")
	it := createShareIntention(t, s, -500, "старые пластинки и треск", memory.ChatScope("-500"))

	allowed, _, err := s.groupInitiativeAllowed(context.Background(), it, time.Now())
	if err != nil {
		t.Fatalf("проверка: %v", err)
	}
	if allowed {
		t.Fatalf("тема, не более близкая группе, чем личке, разрешена")
	}
}

func TestGroupCheckInNeverAllowed(t *testing.T) {
	s := newGroupShareScheduler(t)
	allowed, reason, err := s.groupInitiativeAllowed(context.Background(),
		Intention{ID: 1, Kind: KindCheckIn, TargetChatID: -500}, time.Now())
	if err != nil || allowed {
		t.Fatalf("check_in в группу: allowed=%v reason=%q err=%v", allowed, reason, err)
	}
}
