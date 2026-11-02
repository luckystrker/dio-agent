//go:build db

// Приёмка 17.15: общий потолок карточки end-to-end. Тесты дева проверяют
// лимит оснований (10) на коротких текстах, где бюджет 12К рун не вступает
// в игру; здесь — длинные основания, чтобы потолок карточки сработал на
// реальном пути entityCard: срез хвоста, честный basis_cut, порядок
// «лучшие первыми» (по salience), суммарный объём в бюджете.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// TestAcceptEntityCardBudgetLongTexts — 25 оснований по ~1300 рун: лимит на
// stance оставляет 10 (cut 15), потолок карточки 12К срезает хвост десятки.
func TestAcceptEntityCardBudgetLongTexts(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ts, err := NewToolset(pool, &Searcher{Embedder: nil, Items: Items{}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}

	entity := seedRawEntity(t, ctx, pool, EntityTopic, "Винил", scopeA)
	prov, err := (SourceRepo{}).EnsureProvenance(ctx, pool, SourceJournalEvent, nextRef("prov"), []string{scopeA})
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}

	// Тексты фиксированной длины: "осн NN" + 1290 «у» = 1296 рун.
	textOf := func(i int) string {
		return fmt.Sprintf("осн %02d %s", i, strings.Repeat("у", 1290))
	}
	var basisIDs []int64
	for i := 0; i < 25; i++ {
		it, err := (Items{}).Insert(ctx, pool, NewItem{
			Kind: KindClaim, Text: textOf(i), Salience: 0.9 - float64(i)*0.03, ProvenanceID: prov,
		}, scopeA)
		if err != nil {
			t.Fatalf("вставка основания %d: %v", i, err)
		}
		basisIDs = append(basisIDs, it.ID)
	}
	if _, err := (Stances{}).Upsert(ctx, pool, StanceInput{
		EntityID: entity, Aspect: "general", ScopeID: scopeA,
		Summary: "любит винил", Strength: 0.5, Interest: 0.5,
		BasisMemoryIDs: basisIDs,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	raw, err := ts.entityCard(ctx, scopeA, strconv.FormatInt(entity, 10))
	if err != nil {
		t.Fatalf("entityCard: %v", err)
	}
	var parsed struct {
		Stances []struct {
			Summary  string      `json:"summary"`
			Basis    []basisJSON `json:"basis"`
			BasisCut int         `json:"basis_cut"`
		} `json:"stances"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("карточка не разбирается: %v: %s", err, raw)
	}
	if len(parsed.Stances) != 1 {
		t.Fatalf("stances = %d, хочу 1", len(parsed.Stances))
	}
	st := parsed.Stances[0]

	total := len([]rune(st.Summary))
	for _, b := range st.Basis {
		total += len([]rune(b.Text))
	}
	if total > cardRunesBudget {
		t.Fatalf("карточка вышла за бюджет: %d рун > %d", total, cardRunesBudget)
	}

	// 10 оснований по 1296 рун = 12960 > 12000: потолок обязан срезать хвост
	// десятки, а не отдавать её целиком.
	if len(st.Basis) >= 10 {
		t.Fatalf("оснований %d — потолок карточки не срезал десятку длинных оснований", len(st.Basis))
	}
	// Счётчик честный: 15 срезано лимитом + (10 − показано) срезано потолком.
	wantCut := 15 + (10 - len(st.Basis))
	if st.BasisCut != wantCut {
		t.Fatalf("basis_cut = %d, хочу %d (15 лимитом + %d потолком)", st.BasisCut, wantCut, 10-len(st.Basis))
	}
	// Порядок «лучшие первыми»: показан префикс по salience (первые посеянные).
	for i, b := range st.Basis {
		if b.Text != textOf(i) {
			t.Fatalf("основание %d = %.10s…, хочу %.10s… (префикс по salience)", i, b.Text, textOf(i))
		}
	}
}
