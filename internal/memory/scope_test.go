// Юнит-тесты scope-политики (§9.1–§9.2) — паритет со справочником
// src/memory/scope.test.ts + контракты выдачи записей (ItemVisible).
package memory

import "testing"

func TestChatScope(t *testing.T) {
	if got := ChatScope("111777"); got != "chat:111777" {
		t.Errorf("ChatScope(111777) = %q", got)
	}
	if got := ChatScope("-100200"); got != "chat:-100200" {
		t.Errorf("ChatScope(-100200) = %q", got)
	}
}

func TestIsValidScope(t *testing.T) {
	for _, ok := range []string{"public", "internal", "chat:42", "chat:-100"} {
		if !IsValidScope(ok) {
			t.Errorf("IsValidScope(%q) = false, хочу true", ok)
		}
	}
	for _, bad := range []string{"group", "chat:abc", "", "chat:", "chat:12x", "public "} {
		if IsValidScope(bad) {
			t.Errorf("IsValidScope(%q) = true, хочу false", bad)
		}
	}
}

func TestIntersectScopes(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"chat:1"}, "chat:1"},
		{[]string{"public"}, "public"},
		{nil, "internal"},                            // пусто — internal; missing provenance — internal
		{[]string{}, "internal"},                     //
		{[]string{"internal", "chat:1"}, "internal"}, // internal не расширяет доступ
		{[]string{"public", "chat:1"}, "chat:1"},     // public не сужает
		{[]string{"public", "public"}, "public"},
		{[]string{"chat:1", "chat:2"}, "internal"}, // две приватные без общего чата
		{[]string{"chat:1", "public", "chat:2"}, "internal"},
	}
	for _, c := range cases {
		if got := IntersectScopes(c.in); got != c.want {
			t.Errorf("IntersectScopes(%v) = %q, хочу %q", c.in, got, c.want)
		}
	}
}

func TestVisibleFrom(t *testing.T) {
	if !VisibleFrom("public", "chat:1") || !VisibleFrom("chat:1", "chat:1") {
		t.Error("public виден всем; свой scope виден себе")
	}
	if VisibleFrom("chat:1", "chat:2") {
		t.Error("чужой чат не виден")
	}
	if VisibleFrom("internal", "chat:1") {
		t.Error("internal не виден чатам")
	}
	if !VisibleFrom("internal", "internal") {
		t.Error("internal виден внутреннему контексту")
	}
}

func TestItemVisible(t *testing.T) {
	const aud = "chat:1"

	t.Run("валидное разрешение: свой scope и public видны, чужой и internal нет", func(t *testing.T) {
		if !ItemVisible("chat:1", true, []string{"chat:1"}, aud) {
			t.Error("своя запись с валидным provenance видна")
		}
		if !ItemVisible("public", true, []string{"public"}, aud) {
			t.Error("публичная запись видна")
		}
		if ItemVisible("chat:2", true, []string{"chat:2"}, aud) {
			t.Error("чужая запись не видна")
		}
		if ItemVisible("internal", true, []string{"internal"}, aud) {
			t.Error("internal-запись чату не видна")
		}
	})

	t.Run("missing/invalid provenance → internal (§9.1): чату не выдаётся", func(t *testing.T) {
		if ItemVisible("chat:1", false, nil, aud) {
			t.Error("запись без provenance не видна чату")
		}
		if ItemVisible("chat:1", true, nil, aud) {
			t.Error("запись с пустым разрешением не видна чату")
		}
		if ItemVisible("chat:1", true, []string{"chat:abc"}, aud) {
			t.Error("битый формат элемента портит всё разрешение: чату не выдаётся")
		}
		if ItemVisible("chat:1", true, []string{"chat:1", "мусор"}, aud) {
			t.Error("частично валидное разрешение не частично доверяется")
		}
		if !ItemVisible("chat:1", true, []string{"chat:abc"}, "internal") {
			t.Error("битое разрешение ведёт себя как internal — внутренний контекст видит")
		}
	})

	t.Run("provenance не может расширить назначенный скоуп", func(t *testing.T) {
		// Запись назначена chat:1, разрешение — public: аудитории chat:1 видна,
		// чужим нет; наоборот (public + только chat:1 в provenance) — чужим тоже нет.
		if !ItemVisible("chat:1", true, []string{"public"}, aud) {
			t.Error("валидированное расширение до public выдаёт запись своему чату")
		}
		if ItemVisible("public", true, []string{"chat:1"}, "chat:2") {
			t.Error("provenance сужает: public-запись с chat-only разрешением чужому чату не видна")
		}
	})

	t.Run("аудитория internal видит всё (§9.2: рефлексия отдельно, производные наследуют)", func(t *testing.T) {
		for _, scope := range []string{"public", "internal", "chat:1", "chat:2"} {
			if !ItemVisible(scope, true, []string{scope}, "internal") {
				t.Errorf("internal-аудитория должна видеть scope %q", scope)
			}
		}
		if !ItemVisible("chat:1", false, nil, "internal") {
			t.Error("internal-аудитория видит и записи без provenance")
		}
	})
}
