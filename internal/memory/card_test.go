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

// TestSanitizeCardBasis — 21.6: основание stance в строке карточки обрезается
// своим потолком (150 рун), а не общим потолком заметки 300.
func TestSanitizeCardBasis(t *testing.T) {
	got := sanitizeCardBasis(strings.Repeat("я", cardStanceBasisLimit+50))
	if n := utf8.RuneCountInString(got); n != cardStanceBasisLimit {
		t.Fatalf("основание stance %d рун, хочу %d", n, cardStanceBasisLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("обрезанное основание без многоточия: %q", got)
	}
	// Короткое основание не трогается.
	if short := sanitizeCardBasis("пластинки"); short != "пластинки" {
		t.Fatalf("короткое основание изменено: %q", short)
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
	huge := cardEntry{text: strings.Repeat("важное ", 500), relevant: true}
	card = assembleConversationCard([]cardEntry{relevant, huge}, true)
	if card.Fits {
		t.Fatalf("вытесненное релевантное попадание не пометило карточку неполной")
	}
}

// TestCardRelevantItemsThreshold — отсев семантического шума по порогу:
// full-text попадания релевантны всегда, слабые векторы отбрасываются.
// Срез лишних СЕМАНТИЧЕСКИХ попаданий — нормален (лучшие уже в карточке) и
// полноту не ломает; cut=true только если за лимитом осталось full-text
// попадание.
func TestCardRelevantItemsThreshold(t *testing.T) {
	results := []ScoredItem{
		{Item: Item{ID: 1, Text: "full-text"}, Distance: -1},
		{Item: Item{ID: 2, Text: "настоящее попадание"}, Distance: 0.40},
		{Item: Item{ID: 3, Text: "на границе"}, Distance: 0.5},
		{Item: Item{ID: 4, Text: "далёкий вектор"}, Distance: 0.6},
		{Item: Item{ID: 5, Text: "совсем шум"}, Distance: cardSemanticDistanceMax + 0.1},
	}
	items, cut := cardRelevantItems(results)
	if cut {
		t.Fatalf("маленькая выдача помечена как срезанная")
	}
	if len(items) != 2 || items[0].ID != 1 || items[1].ID != 2 {
		t.Fatalf("порог релевантности не применился: %+v", items)
	}

	// 20 семантических попаданий в диапазоне настоящих совпадений bge-m3:
	// в карточке 8 лучших, полнота не ломается.
	semantic := make([]ScoredItem, 0, 20)
	for i := 0; i < 20; i++ {
		semantic = append(semantic, ScoredItem{
			Item:     Item{ID: int64(100 + i), Text: "x"},
			Distance: 0.30 + 0.14*float64(i)/19,
		})
	}
	items, cut = cardRelevantItems(semantic)
	if cut || len(items) != cardChatItemLimit {
		t.Fatalf("срез лишних семантических: items=%d cut=%v, хочу %d/false", len(items), cut, cardChatItemLimit)
	}

	// Full-text попадание вылетело за лимит — это честная неполнота.
	mixed := append([]ScoredItem{}, semantic[:cardChatItemLimit]...)
	mixed = append(mixed, ScoredItem{Item: Item{ID: 999, Text: "full-text в хвосте"}, Distance: -1})
	items, cut = cardRelevantItems(mixed)
	if !cut || len(items) != cardChatItemLimit {
		t.Fatalf("вытесненное full-text попадание: items=%d cut=%v, хочу %d/true", len(items), cut, cardChatItemLimit)
	}
}
