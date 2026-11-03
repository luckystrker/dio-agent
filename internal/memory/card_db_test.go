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

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "")
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
	again, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "")
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

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "")
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

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "")
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if strings.Index(card.Text, "свежее событие") > strings.Index(card.Text, "старое событие") {
		t.Fatalf("память чата не от свежего к старому:\n%s", card.Text)
	}
}

// TestConversationCardUsesRelevantSearch — этап 21.1: при непустом запросе
// записи карточки выбираются поиском (FTS + вектор), а не «последние N».
// Старая релевантная запись попадает в карточку, хотя её перекрывают свежие
// нерелевантные реплики чата.
func TestConversationCardUsesRelevantSearch(t *testing.T) {
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

	old := seedItem(t, ctx, pool, scopeA, "Данил коллекционирует старые пластинки", seedOpts{
		salience: 0.5, links: []EntityLink{{Name: "Данил", Kind: EntityPerson, Role: RoleSubject}},
	})
	stampRecorded(t, ctx, pool, old.ID, "2026-09-01T10:00:00Z")
	for i := 0; i < 12; i++ {
		fresh := seedItem(t, ctx, pool, scopeA, fmt.Sprintf("свежая болтовня про погоду номер %d", i), seedOpts{
			salience: 0.6, links: []EntityLink{{Name: "Данил", Kind: EntityPerson, Role: RoleSubject}},
		})
		stampRecorded(t, ctx, pool, fresh.ID, "2026-11-01T10:00:00Z")
	}

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "что там с пластинками?")
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if !strings.Contains(card.Text, "пластинки") {
		t.Fatalf("релевантная старая запись не попала в карточку:\n%s", card.Text)
	}
	// Нерелевантные записи в базе не делают карточку неполной (этап 21.2):
	// в память идём только если ответа нет ни в истории, ни в карточке.
	if !card.Fits {
		t.Fatalf("нерелевантные записи сделали карточку неполной:\n%s", card.Text)
	}
	if n := utf8.RuneCountInString(card.Text); n > ConversationCardBudget {
		t.Fatalf("карточка %d рун превышает потолок %d", n, ConversationCardBudget)
	}
}

// TestConversationCardFitsWhenRelevantCut — этап 21.2: срез РЕЛЕВАНТНЫХ
// попаданий по количеству — честная неполнота (ход получит инструменты).
func TestConversationCardFitsWhenRelevantCut(t *testing.T) {
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
	for i := 0; i < cardChatItemLimit+1; i++ {
		seedItem(t, ctx, pool, scopeA, fmt.Sprintf("пластинки выпуск %d", i), seedOpts{salience: 0.5})
	}

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "пластинки")
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if card.Fits {
		t.Fatalf("срезанные релевантные попадания не пометили карточку неполной:\n%s", card.Text)
	}
}

// TestConversationCardSearchFailureFallsBack — сбой поиска/эмбеддинга не
// роняет ход: карточка собирается прежним отбором (последние записи).
func TestConversationCardSearchFailureFallsBack(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ts, err := NewToolset(pool, &Searcher{Embedder: failingEmbedder{}, Items: Items{}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	entity, err := (EntityRepo{}).FindOrCreate(ctx, pool, EntityPerson, "Данил", scopeA)
	if err != nil {
		t.Fatalf("сущность не создана: %v", err)
	}
	seedItem(t, ctx, pool, scopeA, "последняя запись этого чата", seedOpts{salience: 0.5})

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "о чём мы говорили?")
	if err != nil {
		t.Fatalf("сбой поиска уронил сборку карточки: %v", err)
	}
	if !strings.Contains(card.Text, "последняя запись этого чата") {
		t.Fatalf("откат к последним записям не сработал:\n%s", card.Text)
	}
}

// TestConversationCardBasisCountKeepsFits — этап 21.2: несколько оснований
// stance — подтверждение отношения, а не пропущенный факт; карточка полна.
func TestConversationCardBasisCountKeepsFits(t *testing.T) {
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
	prov := seedRawProvenance(t, ctx, pool, []string{scopeA})
	first := seedRawItem(t, ctx, pool, scopeA, "Данил рассказал про пластинки", "active", prov)
	second := seedRawItem(t, ctx, pool, scopeA, "Данил подарил пластинку", "active", prov)
	if _, err := (Stances{}).Upsert(ctx, pool, StanceInput{
		EntityID: entity.ID, Aspect: "general", ScopeID: scopeA,
		Summary: "приятель, интересен", Strength: 0.7, Interest: 0.4,
		BasisMemoryIDs: []int64{first, second},
	}); err != nil {
		t.Fatalf("stance не создан: %v", err)
	}

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "")
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if !card.Fits {
		t.Fatalf("два основания stance сделали карточку неполной:\n%s", card.Text)
	}
}

// failingEmbedder — векторизатор, всегда падающий (сеть недоступна): проверка
// отката карточки к прежнему отбору без падения хода.
type failingEmbedder struct{}

func (failingEmbedder) Model() string { return "failing-test-embedder" }

func (failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, fmt.Errorf("сеть недоступна")
}

// TestConversationCardBudgetFitsStancesAndRelevant — 21.6: карточка 3000 рун
// вмещает две длинные stance-строки (summary + основание) и три релевантные
// записи по 300 рун. На прежних 1500 первое же попадание вылетало, и ход
// получал инструменты на каждом разговоре.
func TestConversationCardBudgetFitsStancesAndRelevant(t *testing.T) {
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
	prov := seedRawProvenance(t, ctx, pool, []string{scopeA})
	basis := seedRawItem(t, ctx, pool, scopeA, strings.Repeat("основание ", 60), "active", prov)
	// Две stance в живом диапазоне ходов 107/108: summary 196 и 313 рун.
	for i, summary := range []string{
		strings.Repeat("а", 196),
		strings.Repeat("б", 313),
	} {
		if _, err := (Stances{}).Upsert(ctx, pool, StanceInput{
			EntityID: entity.ID, Aspect: fmt.Sprintf("aspect-%d", i), ScopeID: scopeA,
			Summary: summary, Strength: 0.7 - 0.1*float64(i), Interest: 0.4,
			BasisMemoryIDs: []int64{basis},
		}); err != nil {
			t.Fatalf("stance %d не создан: %v", i, err)
		}
	}
	// Три релевантные записи по 300 рун.
	for i := 0; i < 3; i++ {
		base := fmt.Sprintf("ремонт номер %d ", i)
		text := base + strings.Repeat("я", 300-utf8.RuneCountInString(base))
		seedItem(t, ctx, pool, scopeA, text, seedOpts{salience: 0.5})
	}

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "ремонт")
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if !card.Fits {
		t.Fatalf("длинные stance и 3 релевантные записи не влезли в 3000 рун:\n%s", card.Text)
	}
	for i := 0; i < 3; i++ {
		if !strings.Contains(card.Text, fmt.Sprintf("ремонт номер %d", i)) {
			t.Fatalf("релевантная запись ремонт номер %d вытеснена из карточки:\n%s", i, card.Text)
		}
	}
	if n := utf8.RuneCountInString(card.Text); n > ConversationCardBudget {
		t.Fatalf("карточка %d рун превышает потолок %d", n, ConversationCardBudget)
	}
}
