//go:build db

// 17.15: потолок объёма выдачи памяти. memory_search отдавал до 50 записей
// полным текстом, карточка сущности — все видимые основания всех stances без
// лимита длины: контекст хода раздувался неограниченно (десятки тысяч рун у
// «ветерана»). Теперь выдача вписывается в бюджет рун, счётчики честные
// («показано N из M», basis_cut), порядок «лучшие первыми» сохранён.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// TestToolSearchOutputRunesBudget — длинная выдача обрезана по суммарному
// объёму текстов, счётчик «показано N из M» честный, порядок ранжирования
// (лучшие первыми) сохранён: показан префикс выдачи, а не случайный срез.
func TestToolSearchOutputRunesBudget(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ts, err := NewToolset(pool, &Searcher{Embedder: nil, Items: Items{}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}

	// 30 записей по ~670 рун, каждая отвечает запросу; salience строго
	// убывает — ранжирование детерминировано (ts_rank у всех одинаков).
	prov, err := (SourceRepo{}).EnsureProvenance(ctx, pool, SourceJournalEvent, nextRef("prov"), []string{scopeA})
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	filler := strings.Repeat("а", 650)
	var ids []string
	for i := 0; i < 30; i++ {
		it, err := (Items{}).Insert(ctx, pool, NewItem{
			Kind: KindClaim, Text: fmt.Sprintf("уникум запись номер %d %s", i, filler),
			Salience: 0.95 - float64(i)*0.01, ProvenanceID: prov,
		}, scopeA)
		if err != nil {
			t.Fatalf("вставка %d: %v", i, err)
		}
		ids = append(ids, strconv.FormatInt(it.ID, 10))
	}

	limit := 50
	raw, err := ts.searchMemory(ctx, scopeA, "уникум", &limit)
	if err != nil {
		t.Fatalf("searchMemory: %v", err)
	}
	var parsed struct {
		Items []itemJSON `json:"items"`
		Shown string     `json:"shown"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("выдача не разбирается: %v: %s", err, raw)
	}
	if len(parsed.Items) == 0 || len(parsed.Items) >= 30 {
		t.Fatalf("показано %d записей из 30 — потолок объёма не сработал", len(parsed.Items))
	}
	total := 0
	for _, it := range parsed.Items {
		total += len([]rune(it.Text))
	}
	if total > toolSearchRunesBudget {
		t.Fatalf("суммарный объём %d рун выше бюджета %d", total, toolSearchRunesBudget)
	}
	want := fmt.Sprintf("показано %d из %d", len(parsed.Items), 30)
	if parsed.Shown != want {
		t.Fatalf("счётчик = %q, хочу %q", parsed.Shown, want)
	}
	// Порядок «лучшие первыми»: выдача — префикс ранжирования.
	for i, it := range parsed.Items {
		if it.ID != ids[i] {
			t.Fatalf("позиция %d: запись %s, хочу лучшую %s — порядок срезан", i, it.ID, ids[i])
		}
	}
}

// TestToolEntityCardBasisLimit — оснований в карточке не больше лимита,
// отобраны самые значимые (по salience), счётчик среза честный.
func TestToolEntityCardBasisLimit(t *testing.T) {
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
	var basisIDs []int64
	for i := 0; i < 25; i++ {
		it, err := (Items{}).Insert(ctx, pool, NewItem{
			Kind: KindClaim, Text: fmt.Sprintf("основание винила номер %02d", i),
			Salience: 0.9 - float64(i)*0.03, ProvenanceID: prov,
		}, scopeA)
		if err != nil {
			t.Fatalf("вставка %d: %v", i, err)
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
	if len(st.Basis) != stanceBasisLimit {
		t.Fatalf("оснований %d, хочу лимит %d", len(st.Basis), stanceBasisLimit)
	}
	if st.BasisCut != 15 {
		t.Fatalf("basis_cut = %d, хочу 15 (честный счётчик среза)", st.BasisCut)
	}
	// Отобраны самые значимые: топ по salience — первые посеянные.
	for i, b := range st.Basis {
		want := fmt.Sprintf("основание винила номер %02d", i)
		if b.Text != want {
			t.Fatalf("основание %d = %q, хочу %q (по salience)", i, b.Text, want)
		}
	}
}
