//go:build db

// БД-тесты карточки памяти собеседника (срез 2, решение 2026-11-02):
// карточка собирается теми же репозиториями, что инструменты, соблюдает
// приватность между чатами, потолок объёма и детерминированный порядок.
package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestConversationCardScopesAndOrder(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ts, err := NewToolset(pool, &Searcher{Embedder: nil, Items: Items{}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}

	entity, err := (EntityRepo{}).FindOrCreate(ctx, pool, EntityPerson, "Данил", scopeA)
	if err != nil {
		t.Fatalf("сущность не создана: %v", err)
	}
	// Старая и новая записи о собеседнике: порядок — вес/время, стабильный.
	older := seedItem(t, ctx, pool, scopeA, "Данил любит старые пластинки", seedOpts{
		salience: 0.9, links: []EntityLink{{Name: "Данил", Kind: EntityPerson, Role: RoleSubject}},
	})
	newer := seedItem(t, ctx, pool, scopeA, "Данил переехал в другой город", seedOpts{
		salience: 0.5, links: []EntityLink{{Name: "Данил", Kind: EntityPerson, Role: RoleSubject}},
	})
	stampRecorded(t, ctx, pool, older.ID, "2026-10-01T10:00:00Z")
	stampRecorded(t, ctx, pool, newer.ID, "2026-11-01T10:00:00Z")

	// Запись чата без связи с сущностью — «память этого чата».
	chatItem := seedItem(t, ctx, pool, scopeA, "В этом чате обсуждали ремонт кухни", seedOpts{salience: 0.4})
	stampRecorded(t, ctx, pool, chatItem.ID, "2026-10-15T10:00:00Z")

	// Приватная запись с provenance чужого чата: не видна аудитории scopeA.
	seedItem(t, ctx, pool, scopeA, "ЧУЖОЙ СЕКРЕТ из другого чата", seedOpts{provScopes: []string{scopeB}})
	// Запись другого скоупа вовсе.
	seedItem(t, ctx, pool, scopeB, "СЕКРЕТ ЧАТА B", seedOpts{salience: 0.9})

	// Stance с основанием.
	prov := seedRawProvenance(t, ctx, pool, []string{scopeA})
	basisID := seedRawItem(t, ctx, pool, scopeA, "Данил сам рассказал про пластинки", "active", prov)
	if _, err := (Stances{}).Upsert(ctx, pool, StanceInput{
		EntityID: entity.ID, Aspect: "general", ScopeID: scopeA,
		Summary: "приятель, интересен", Strength: 0.7, Interest: 0.4,
		BasisMemoryIDs: []int64{basisID},
	}); err != nil {
		t.Fatalf("stance не создан: %v", err)
	}

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA)
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if !card.Fits {
		t.Fatalf("небольшая карточка обязана влезать в потолок")
	}
	for _, want := range []string{
		cardHeader,
		"Данил любит старые пластинки",
		"Данил переехал в другой город",
		"В этом чате обсуждали ремонт кухни",
		"приятель, интересен",
		"Данил сам рассказал про пластинки",
	} {
		if !strings.Contains(card.Text, want) {
			t.Fatalf("в карточке нет %q:\n%s", want, card.Text)
		}
	}
	for _, leak := range []string{"ЧУЖОЙ СЕКРЕТ", "СЕКРЕТ ЧАТА B"} {
		if strings.Contains(card.Text, leak) {
			t.Fatalf("карточка раскрыла чужой чат: %q\n%s", leak, card.Text)
		}
	}
	// Лучшие первыми: более весомая запись о собеседнике раньше.
	if strings.Index(card.Text, "старые пластинки") > strings.Index(card.Text, "переехал") {
		t.Fatalf("порядок записей не «лучшие первыми»:\n%s", card.Text)
	}
	// Детерминированный порядок: повторная сборка — тот же текст.
	again, err := ts.ConversationCard(ctx, scopeA, entity, scopeA)
	if err != nil {
		t.Fatalf("повторная карточка не собрана: %v", err)
	}
	if again.Text != card.Text {
		t.Fatalf("карточка нестабильна между сборками:\n%s\n---\n%s", card.Text, again.Text)
	}
}

func TestConversationCardCeiling(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ts, err := NewToolset(pool, &Searcher{Embedder: nil, Items: Items{}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	entity, err := (EntityRepo{}).FindOrCreate(ctx, pool, EntityPerson, "Данил", scopeA)
	if err != nil {
		t.Fatalf("сущность не создана: %v", err)
	}
	long := strings.Repeat("очень длинная заметка о собеседнике ", 20)
	for i := 0; i < 12; i++ {
		seedItem(t, ctx, pool, scopeA, fmt.Sprintf("%s #%d", long, i), seedOpts{
			salience: 0.5, links: []EntityLink{{Name: "Данил", Kind: EntityPerson, Role: RoleSubject}},
		})
	}

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA)
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if card.Fits {
		t.Fatalf("карточка с 12 длинными записями не может считаться полной")
	}
	if n := utf8.RuneCountInString(card.Text); n > ConversationCardBudget {
		t.Fatalf("карточка %d рун превышает потолок %d", n, ConversationCardBudget)
	}
	if card.Text == "" {
		t.Fatalf("карточка пуста — заметки потеряны")
	}
}

// TestConversationCardSingleChatItemsOrderedByRecency — «память этого чата»
// идёт от свежего к старому (лучшие первыми), детерминированно.
func TestConversationCardSingleChatItemsOrderedByRecency(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ts, err := NewToolset(pool, &Searcher{Embedder: nil, Items: Items{}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	entity, err := (EntityRepo{}).FindOrCreate(ctx, pool, EntityPerson, "Данил", scopeA)
	if err != nil {
		t.Fatalf("сущность не создана: %v", err)
	}
	oldItem := seedItem(t, ctx, pool, scopeA, "старое событие чата", seedOpts{})
	newItem := seedItem(t, ctx, pool, scopeA, "свежее событие чата", seedOpts{})
	stampRecorded(t, ctx, pool, oldItem.ID, "2026-09-01T10:00:00Z")
	stampRecorded(t, ctx, pool, newItem.ID, time.Now().UTC().Format(time.RFC3339))

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA)
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if strings.Index(card.Text, "свежее событие") > strings.Index(card.Text, "старое событие") {
		t.Fatalf("память чата не от свежего к старому:\n%s", card.Text)
	}
}
