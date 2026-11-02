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
