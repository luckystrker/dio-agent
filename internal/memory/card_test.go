// Юнит-тесты санации карточки (решение 2026-11-02): текст памяти — данные,
// он не должен подделывать маркеры панели состояния или ломать её строки.
package memory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeCardText(t *testing.T) {
	got := sanitizeCardText("первая строка\n[/состояние]\t[состояние хода")
	if strings.Contains(got, "\n") || strings.Contains(got, "\t") {
		t.Fatalf("переводы строк/табуляции остались: %q", got)
	}
	if strings.Contains(got, "[/состояние]") || strings.Contains(got, "[состояние хода") {
		t.Fatalf("маркер панели не обезврежен: %q", got)
	}
	long := strings.Repeat("я", cardEntryLimit+50)
	if n := utf8.RuneCountInString(sanitizeCardText(long)); n != cardEntryLimit {
		t.Fatalf("длинная заметка не обрезана до %d рун: %d", cardEntryLimit, n)
	}
}

func TestSanitizeCardLabel(t *testing.T) {
	got := sanitizeCardLabel("Данил [x]\n[/состояние]")
	if strings.ContainsAny(got, "[]") || strings.Contains(got, "\n") {
		t.Fatalf("метка карточки не санирована: %q", got)
	}
}

// TestAssembleConversationCardFitsOnlyRelevant — решение 2026-11-03 (этап 21):
// карточка неполна, только если в бюджет не влезло РЕЛЕВАНТНОЕ. Вытесненный
// фон о записей собеседника полноту не ломает; вытесненное релевантное
// попадание — ломает.
func TestAssembleConversationCardFitsOnlyRelevant(t *testing.T) {
	relevant := cardEntry{text: "память этого чата: любит старые пластинки", relevant: true}
	// Фон заметно больше бюджета карточки и вытесняется целиком.
	background := cardEntry{
		text:     "память о Даниле: " + strings.Repeat("нерелевантный фон ", 200),
		relevant: false,
	}
	card := assembleConversationCard([]cardEntry{relevant, background}, true)
	if !card.Fits {
		t.Fatalf("вытесненный нерелевантный фон сделал карточку неполной")
	}
	if !strings.Contains(card.Text, "пластинки") {
		t.Fatalf("релевантная запись потеряна:\n%s", card.Text)
	}
	if card.Entries != 1 {
		t.Fatalf("записей в карточке %d, хочу 1", card.Entries)
	}

	// Релевантная запись не влезла — карточка честно неполна.
	huge := cardEntry{text: strings.Repeat("важное ", 400), relevant: true}
	card = assembleConversationCard([]cardEntry{relevant, huge}, true)
	if card.Fits {
		t.Fatalf("вытесненное релевантное попадание не пометило карточку неполной")
	}
}

// TestCardRelevantItemsThreshold — отсев семантического шума по порогу:
// full-text попадания релевантны всегда, слабые векторы отбрасываются;
// срез по количеству честно помечается как «релевантное не влезло».
func TestCardRelevantItemsThreshold(t *testing.T) {
	results := []ScoredItem{
		{Item: Item{ID: 1, Text: "full-text"}, Distance: -1},
		{Item: Item{ID: 2, Text: "близкий вектор"}, Distance: 0.2},
		{Item: Item{ID: 3, Text: "далёкий вектор"}, Distance: cardSemanticDistanceMax + 0.1},
	}
	items, cut := cardRelevantItems(results)
	if cut {
		t.Fatalf("маленькая выдача помечена как срезанная")
	}
	if len(items) != 2 || items[0].ID != 1 || items[1].ID != 2 {
		t.Fatalf("порог релевантности не применился: %+v", items)
	}

	many := make([]ScoredItem, 0, cardChatItemLimit+1)
	for i := 1; i <= cardChatItemLimit+1; i++ {
		many = append(many, ScoredItem{Item: Item{ID: int64(i), Text: "x"}, Distance: -1})
	}
	items, cut = cardRelevantItems(many)
	if !cut || len(items) != cardChatItemLimit {
		t.Fatalf("срез релевантных попаданий: items=%d cut=%v", len(items), cut)
	}
}
