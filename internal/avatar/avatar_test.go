// Юнит-тесты текста записи об аватарке (34.2).
package avatar

import (
	"strings"
	"testing"
)

func TestAvatarMemoryText(t *testing.T) {
	if got := DescriptionText("Коля", "кот в солнечных очках, весёлый"); got != "у Коля на аватарке кот в солнечных очках, весёлый" {
		t.Fatalf("описание: %q", got)
	}
	if got := DescriptionText("", "море"); got != "у собеседника на аватарке море" {
		t.Fatalf("без имени: %q", got)
	}
	if got := InvisibleText("Коля"); got != "у Коля не видно аватарки" {
		t.Fatalf("не видно: %q", got)
	}
	long := strings.Repeat("я", DescriptionLimit+30)
	got := normalizeDescription(long)
	if len([]rune(got)) != DescriptionLimit {
		t.Fatalf("описание не обрезано: %d рун", len([]rune(got)))
	}
	if got := normalizeDescription("  «кот\nв очках»  "); got != "кот в очках" {
		t.Fatalf("нормализация: %q", got)
	}
	if got := normalizeDescription("   "); got == "" {
		t.Fatalf("пустое описание не должно давать пустую запись")
	}
}
