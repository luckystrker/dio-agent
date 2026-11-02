// Приёмка 17.15, раунд 2: «ветеранская» карточка — десятки stances с полными
// комплектами оснований. Тесты дева и раунда 1 гонят потолок карточки на ОДНОЙ
// stance с длинными основаниями; здесь — много stances: общий потолок обязан
// резать целые stances с конца, а не только основания внутри первой.
package memory

import (
	"fmt"
	"strings"
	"testing"
)

// TestAcceptTrimCardManyStances — 40 stances × (заголовок + 10 оснований по
// ~700 рун) ≈ 280К рун против бюджета 12К: карточка остаётся префиксом
// списка stances, суммарный объём — в бюджете, заголовок первой stance и её
// первое основание живут при любом бюджете.
func TestAcceptTrimCardManyStances(t *testing.T) {
	stances := make([]stanceJSON, 40)
	for i := range stances {
		st := stanceJSON{
			Summary: fmt.Sprintf("мнение %02d ", i) + strings.Repeat("s", 50),
		}
		for b := 0; b < 10; b++ {
			st.Basis = append(st.Basis, basisJSON{Text: strings.Repeat("о", 700)})
		}
		stances[i] = st
	}
	memories := []itemJSON{
		itemOf("m1", strings.Repeat("м", 600)),
		itemOf("m2", strings.Repeat("н", 600)),
	}

	gotSt, gotMem := trimCardToBudget(stances, memories, cardRunesBudget)

	total := 0
	for _, st := range gotSt {
		total += len([]rune(st.Summary))
		for _, b := range st.Basis {
			total += len([]rune(b.Text))
		}
	}
	for _, m := range gotMem {
		total += len([]rune(m.Text))
	}
	if total > cardRunesBudget+1000 { // заголовок первой stance переживает бюджет — допуск на него
		t.Fatalf("ветеранская карточка вышла из-под бюджета: %d рун (бюджет %d)", total, cardRunesBudget)
	}
	if len(gotSt) < 1 || gotSt[0].Summary != stances[0].Summary {
		t.Fatalf("первая stance исчезла: %+v", gotSt)
	}
	if len(gotSt[0].Basis) < 1 {
		t.Fatalf("первое основание первой stance не выжило: %+v", gotSt[0])
	}
	// Срез — хвост: оставшиеся stances идут в исходном порядке без дыр.
	for i := 1; i < len(gotSt); i++ {
		if gotSt[i].Summary != stances[i].Summary {
			t.Fatalf("stance %d не из исходного порядка — срез не хвост", i)
		}
	}
	if len(gotSt) >= 40 {
		t.Fatalf("40 stances по 7К рун влезли в бюджет %d — потолок не работает", cardRunesBudget)
	}
}
