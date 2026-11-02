// Юнит-тесты потолков объёма выдачи (17.15): чистые функции над JSON-формами,
// без БД. Поведенческие проверки end-to-end — в tools_limits_db_test.go.
package memory

import (
	"strings"
	"testing"
)

func itemOf(id, text string) itemJSON { return itemJSON{ID: id, Text: text} }

// TestCapSearchItemsRunes — бюджет по сумме рун текстов, порядок сохранён
// (префикс ранжирования), первая запись показывается даже одиночно сверх
// бюджета.
func TestCapSearchItemsRunes(t *testing.T) {
	items := []itemJSON{
		itemOf("1", strings.Repeat("а", 100)),
		itemOf("2", strings.Repeat("б", 100)),
		itemOf("3", strings.Repeat("в", 100)),
	}

	kept := capSearchItems(items, 250)
	if len(kept) != 2 || kept[0].ID != "1" || kept[1].ID != "2" {
		t.Fatalf("бюджет 250: %+v, хочу первые две записи", kept)
	}

	kept = capSearchItems(items, 300)
	if len(kept) != 3 {
		t.Fatalf("бюджет 300: %+v, хочу все три", kept)
	}

	// Первая запись больше бюджета — показывается, выдача не пустая.
	big := []itemJSON{itemOf("1", strings.Repeat("а", 500))}
	if kept = capSearchItems(big, 100); len(kept) != 1 {
		t.Fatalf("одиночная запись сверх бюджета: %+v, хочу её же", kept)
	}

	if kept = capSearchItems(nil, 100); len(kept) != 0 {
		t.Fatalf("пустой вход: %+v", kept)
	}
}

// TestTrimCardToBudget — общий потолок карточки: основания режутся с конца
// внутри stance (срез честно в basis_cut), записи memories — с конца, целые
// stances уходят с конца; заголовок первой stance и её первое основание
// остаются всегда.
func TestTrimCardToBudget(t *testing.T) {
	t600 := func(s string) string { return s + strings.Repeat("х", 600) }
	stances := []stanceJSON{
		{Summary: "первая.", Basis: []basisJSON{{Text: t600("a")}, {Text: t600("b")}}},
		{Summary: "вторая.", Basis: []basisJSON{{Text: t600("c")}}},
	}
	memories := []itemJSON{
		itemOf("m1", strings.Repeat("м", 500)),
		itemOf("m2", strings.Repeat("н", 700)),
	}

	// Бюджет 1800: первая stance целиком (10+600+600), заголовок второй (10),
	// основание второй не влезает (basis_cut=1), первая запись памяти (500)
	// влезает, вторая (700) — нет.
	gotSt, gotMem := trimCardToBudget(stances, memories, 1800)
	if len(gotSt) != 2 {
		t.Fatalf("stances = %d, хочу 2 (заголовки остаются)", len(gotSt))
	}
	if len(gotSt[0].Basis) != 2 || gotSt[0].BasisCut != 0 {
		t.Fatalf("первая stance: %+v cut=%d, хочу 2 основания без среза", gotSt[0].Basis, gotSt[0].BasisCut)
	}
	if len(gotSt[1].Basis) != 0 || gotSt[1].BasisCut != 1 {
		t.Fatalf("вторая stance: basis=%d cut=%d, хочу 0 и 1", len(gotSt[1].Basis), gotSt[1].BasisCut)
	}
	if len(gotMem) != 1 || gotMem[0].ID != "m1" {
		t.Fatalf("memories = %+v, хочу первую", gotMem)
	}

	// Всё влезает — ничего не режется.
	gotSt, gotMem = trimCardToBudget(stances, memories, 10000)
	if len(gotSt) != 2 || len(gotSt[0].Basis) != 2 || len(gotSt[1].Basis) != 1 || len(gotMem) != 2 {
		t.Fatalf("большой бюджет что-то срезал: st=%+v mem=%+v", gotSt, gotMem)
	}

	// Крошечный бюджет: остаётся только первая stance — заголовок и первое
	// основание всегда, остальные уходят целиком.
	gotSt, gotMem = trimCardToBudget(stances, memories, 100)
	if len(gotSt) != 1 || gotSt[0].Summary != "первая." || len(gotSt[0].Basis) != 1 {
		t.Fatalf("крошечный бюджет: st=%+v, хочу первую stance с одним основанием", gotSt)
	}
	if len(gotMem) != 0 {
		t.Fatalf("крошечный бюджет: memories = %+v, хочу пусто", gotMem)
	}
}

// TestTrimStanceBasis — лимит оснований: срез — хвост (Basis отсортирован
// по salience в SQL), счётчик среза возвращается честно.
func TestTrimStanceBasis(t *testing.T) {
	basis := make([]Item, 12)
	for i := range basis {
		basis[i] = Item{ID: int64(i)}
	}
	kept, cut := trimStanceBasis(basis, 10)
	if len(kept) != 10 || cut != 2 {
		t.Fatalf("kept=%d cut=%d, хочу 10 и 2", len(kept), cut)
	}
	if kept[0].ID != 0 {
		t.Fatalf("срезан не хвост: первый = %d", kept[0].ID)
	}
	short := basis[:3]
	if kept, cut = trimStanceBasis(short, 10); len(kept) != 3 || cut != 0 {
		t.Fatalf("короткий список срезан: kept=%d cut=%d", len(kept), cut)
	}
}
