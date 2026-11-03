//go:build db

// БД-тесты этапа 28.3: почему карточка неполна. Живые ходы 122/126: 21 запись
// памяти, в чате частые слова «ремонт», «шпаклёвка», «кот» — полнотекст с
// OR-семантикой находил 9 записей, девятая не влезала в потолок 8, карточка
// объявлялась неполной, включались инструменты памяти и сжигали лимит 2.
// С 28.3 неполнота — только за ДЕЙСТВИТЕЛЬНО релевантное: полнотекстовое
// попадание считается релевантным, если покрывает заметную долю запроса
// (ts_rank относительно лучшего достижимого), а причина неполноты называет
// ветку (fts_cut | budget | stances | limit).
package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// referenceNoisyChatData — 21 запись «живого» чата: частые слова ремонт/
// шпаклёвка/кот, длина как у настоящих заметок.
func referenceNoisyChatData() []string {
	return []string{
		"ремонт кухни: шпаклёвка сохнет плохо, кот постоянно лезет в ведро",
		"кот опять спит на подоконнике и скинул цветок",
		"шпаклёвка на стене треснула после высыхания",
		"сегодня докупил шпаклёвку и грунтовку для ремонта",
		"кот отказался есть новый корм",
		"ремонт идёт медленно, рабочие не пришли",
		"кот залез в шкаф и свалил банку",
		"шпаклёвка закончилась, надо ещё мешок",
		"обсуждали ремонт коридора и кухни",
		"кот громко мяукал всю ночь",
		"ремонт соседей не даёт спать",
		"шпаклёвка плохо ложится на старую краску",
		"кот поцарапал диван",
		"ремонт ванной отложили до весны",
		"шпаклёвка сохнет быстрее с обогревателем",
		"кот спрятался под ванной",
		"ремонт кухни почти закончен",
		"шпаклёвка и краска остались после ремонта",
		"кот любит сидеть на подоконнике",
		"ремонт начался с замены труб",
		"кот стащил кусок колбасы со стола",
	}
}

const referenceNoisyQuery = "слушай а мы вчера опять возились с ремонтом, шпаклёвка никак не сохнет, кот опять залез в ведро и всё испортил, что делать"

// TestConversationCardFrequentWordsNotIncomplete — воспроизведение живого хода:
// 21 запись с частыми словами не делает карточку неполной. Лучшие по покрытию
// запроса записи попадают в карточку, а случайное совпадение одного слова из
// длинной пачки — не «действительно релевантное».
func TestConversationCardFrequentWordsNotIncomplete(t *testing.T) {
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
	for _, text := range referenceNoisyChatData() {
		seedItem(t, ctx, pool, scopeA, text, seedOpts{salience: 0.5})
	}

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, referenceNoisyQuery)
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if !card.Fits {
		t.Fatalf("частые слова сделали карточку неполной (причина %q):\n%s", card.Incomplete, card.Text)
	}
	if card.Incomplete != "" {
		t.Fatalf("полная карточка назвала причину неполноты %q", card.Incomplete)
	}
	// Запись, покрывающая три слова запроса, — в карточке и впереди случайных.
	if !strings.Contains(card.Text, "ремонт кухни: шпаклёвка сохнет плохо") {
		t.Fatalf("лучшее по покрытию попадание вытеснено:\n%s", card.Text)
	}
	if n := utf8.RuneCountInString(card.Text); n > ConversationCardBudget {
		t.Fatalf("карточка %d рун превышает потолок %d", n, ConversationCardBudget)
	}
	if card.Entries > cardChatItemLimit+cardStanceLimit+cardActorBackgroundLimit {
		t.Fatalf("заметок %d — больше, чем суммарные потолки карточки", card.Entries)
	}
}

// TestConversationCardHighCoverageCutReason — настоящий срез: девять записей
// покрывают запрос целиком, в карточку влезает восемь — это честная неполнота
// с причиной fts_cut (без инструментов девятую не достать).
func TestConversationCardHighCoverageCutReason(t *testing.T) {
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
		seedItem(t, ctx, pool, scopeA,
			fmt.Sprintf("ремонт шпаклёвка кот выпуск %d", i), seedOpts{salience: 0.5})
	}

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "ремонт шпаклёвка кот")
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if card.Fits {
		t.Fatalf("срез девяти высокорелевантных попаданий не пометил карточку неполной:\n%s", card.Text)
	}
	if !strings.Contains(card.Incomplete, "fts_cut") {
		t.Fatalf("причина неполноты %q, хочу fts_cut", card.Incomplete)
	}
}

// TestConversationCardBudgetReason — релевантное не влезло в потолок объёма:
// причина budget, а не общая «карточка неполна».
func TestConversationCardBudgetReason(t *testing.T) {
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
	long := strings.Repeat("очень длинная заметка о собеседнике ", 20) // ~740 рун
	for i := 0; i < cardActorItemLimit; i++ {
		seedItem(t, ctx, pool, scopeA, fmt.Sprintf("%s #%d", long, i), seedOpts{
			salience: 0.5, links: []EntityLink{{Name: "Данил", Kind: EntityPerson, Role: RoleSubject}},
		})
	}
	// Три stance-строки съедают бюджет до записей — записи собеседника не
	// влезают, а они релевантны в прежнем отборе (пустой запрос).
	for i := 0; i < cardStanceLimit; i++ {
		if _, err := (Stances{}).Upsert(ctx, pool, StanceInput{
			EntityID: entity.ID, Aspect: fmt.Sprintf("aspect-%d", i), ScopeID: scopeA,
			Summary: strings.Repeat("отношение ", 30), Strength: 0.7 - 0.1*float64(i), Interest: 0.4,
		}); err != nil {
			t.Fatalf("stance %d не создан: %v", i, err)
		}
	}

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "")
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if card.Fits {
		t.Fatalf("длинные записи не пометили карточку неполной:\n%s", card.Text)
	}
	if !strings.Contains(card.Incomplete, "budget") {
		t.Fatalf("причина неполноты %q, хочу budget", card.Incomplete)
	}
}

// TestConversationCardStancesReason — отношений больше потолка: причина stances.
func TestConversationCardStancesReason(t *testing.T) {
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
	for i := 0; i < cardStanceLimit+1; i++ {
		if _, err := (Stances{}).Upsert(ctx, pool, StanceInput{
			EntityID: entity.ID, Aspect: fmt.Sprintf("aspect-%d", i), ScopeID: scopeA,
			Summary: "отношение", Strength: 0.7 - 0.1*float64(i), Interest: 0.4,
		}); err != nil {
			t.Fatalf("stance %d не создан: %v", i, err)
		}
	}

	card, err := ts.ConversationCard(ctx, scopeA, entity, scopeA, "")
	if err != nil {
		t.Fatalf("карточка не собрана: %v", err)
	}
	if card.Fits {
		t.Fatal("лишние stances не пометили карточку неполной")
	}
	if !strings.Contains(card.Incomplete, "stances") {
		t.Fatalf("причина неполноты %q, хочу stances", card.Incomplete)
	}
}
