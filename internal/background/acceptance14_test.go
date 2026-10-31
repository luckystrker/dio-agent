package background

// Этап 14.1 (§17): контракт sessionForChat — чистой функции, строящей сессию
// провайдера из chat id. Ожидания — из спеки, не из тела: групповые чаты
// Telegram имеют ОТРИЦАТЕЛЬНЫЕ id и получают сессию наравне с личными
// (непустой стабильный хеш для любого ненулевого id, пустая строка только
// для 0); наружу идёт только хеш — сам chat id в значении не читается.
import (
	"strconv"
	"strings"
	"testing"
)

func TestSessionForChatContract(t *testing.T) {
	t.Run("группа (отрицательный id): непустая сессия", func(t *testing.T) {
		if got := sessionForChat(-200700); got == "" {
			t.Fatal("групповой чат Telegram (отрицательный id) обязан получать сессию наравне с личным (14.1)")
		}
	})

	t.Run("личный чат (положительный id): непустая сессия", func(t *testing.T) {
		if got := sessionForChat(4242); got == "" {
			t.Fatal("личный чат обязан получать сессию")
		}
	})

	t.Run("стабильность: тот же вход — тот же выход", func(t *testing.T) {
		for _, id := range []int64{-200700, 4242} {
			a, b := sessionForChat(id), sessionForChat(id)
			if a != b {
				t.Fatalf("chat %d: сессия нестабильна: %q != %q", id, a, b)
			}
		}
	})

	t.Run("нулевой chat id — пустая строка", func(t *testing.T) {
		if got := sessionForChat(0); got != "" {
			t.Fatalf("chat 0: сессия %q, хочу пустую строку (нет разговора — нет заголовка)", got)
		}
	})

	t.Run("наружу хеш, а не сам chat id (§17)", func(t *testing.T) {
		for _, id := range []int64{-200700, 4242} {
			sess := sessionForChat(id)
			signed := strconv.FormatInt(id, 10)
			for _, form := range []string{signed, strings.TrimPrefix(signed, "-")} {
				if strings.Contains(sess, form) {
					t.Fatalf("chat %d: сессия %q содержит id в открытом виде (%q) — наружу идёт только хеш (§17)", id, sess, form)
				}
			}
		}
	})

	t.Run("разные разговоры — разные сессии", func(t *testing.T) {
		seen := map[string]int64{}
		for _, id := range []int64{-200700, 200700, 4242, -4242} {
			sess := sessionForChat(id)
			if prev, dup := seen[sess]; dup {
				t.Fatalf("чаты %d и %d получили одну сессию %q — идентификатор разговора обязан различать разговоры", prev, id, sess)
			}
			seen[sess] = id
		}
	})
}
