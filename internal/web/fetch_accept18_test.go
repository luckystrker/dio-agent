// Дополнительные проверки приёмки блока 18.9/18.10 (тестировщик): сценарии,
// не покрытые тестами разработчика, — IPv4-mapped форма запрещённых адресов,
// честность маркера обрезки у границы, регистронезависимость charset и таблица
// cp1251 на непрерывность кириллического блока.
package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	neturl "net/url"
	"strings"
	"testing"
	"time"
)

// TestAddrAllowedUnmapsIPv4MappedAddresses — CGNAT в IPv4-mapped форме
// (::ffff:100.64.0.1) обязан попадать под тот же запрет, что и обычная форма
// (блок 18.10): netip.Prefix.Contains не матчит 4-in-6 адреса о в4-префикс,
// поэтому guard обязан нормализовать адрес (Unmap) до проверки. Публичный
// адрес в mapped форме остаётся разрешённым.
func TestAddrAllowedUnmapsIPv4MappedAddresses(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"::ffff:100.64.0.1", false},      // CGNAT 100.64.0.0/10 в mapped форме (18.10)
		{"::ffff:100.127.255.254", false}, // верх CGNAT в mapped форме (18.10)
		{"::ffff:8.8.8.8", true},          // публичный в mapped форме — разрешён
	}
	for _, tc := range cases {
		if got := addrAllowed(netip.MustParseAddr(tc.ip)); got != tc.want {
			t.Errorf("%s: addrAllowed=%v, хочу %v (18.10: mapped-форма запрещённого префикса)", tc.ip, got, tc.want)
		}
	}
}

// TestValidateURLRejectsMappedCGNATHostLiteral — URL-уровень guard'а: хост-
// литерал [::ffff:100.64.0.1] обязан отклоняться до соединения (§15 + 18.10).
func TestValidateURLRejectsMappedCGNATHostLiteral(t *testing.T) {
	u, err := neturl.Parse("http://[::ffff:100.64.0.1]/x")
	if err != nil {
		t.Fatalf("URL не разобран: %v", err)
	}
	if err := NewClient(0, time.Second).validateURLOnly(u); err == nil {
		t.Error("хост-литерал [::ffff:100.64.0.1] обязан отклоняться (18.10: CGNAT в mapped форме)")
	}
}

// TestFetchNoTruncationMarkerWhenUnderLimit — маркер «[страница обрезана]»
// появляется ТОЛЬКО при реальной обрезке: короткий текст и текст ровно в
// MaxTextRunes рун уходят целиком без пометки (блок 18.9: честный маркер).
func TestFetchNoTruncationMarkerWhenUnderLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("короткий текст"))
	}))
	defer srv.Close()

	page, err := loopbackClient(0).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch упал: %v", err)
	}
	if page.Text != "короткий текст" {
		t.Fatalf("короткий текст обязан проходить целиком: %q", page.Text)
	}
	if strings.Contains(page.Text, truncationMarker) {
		t.Fatalf("маркер обрезки не должен появляться без обрезки: %q", page.Text)
	}

	// Ровно на границе MaxTextRunes рун — обрезки нет, маркера нет.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(strings.Repeat("а", MaxTextRunes)))
	}))
	defer srv2.Close()

	page2, err := loopbackClient(0).Fetch(context.Background(), srv2.URL)
	if err != nil {
		t.Fatalf("fetch упал: %v", err)
	}
	if got := len([]rune(page2.Text)); got != MaxTextRunes {
		t.Fatalf("текст ровно в %d рун обязан проходить целиком, в выдаче %d рун", MaxTextRunes, got)
	}
	if strings.Contains(page2.Text, truncationMarker) {
		t.Fatalf("маркер обрезки не должен появляться на точной границе (блок 18.9): %q",
			tail(page2.Text, 30))
	}
}

// TestFetchCharsetHeaderCaseInsensitive — charset в заголовке Content-Type
// регистронезависим: «charset=Windows-1251» декодируется так же, как
// «charset=windows-1251» (блок 18.9).
func TestFetchCharsetHeaderCaseInsensitive(t *testing.T) {
	// "Привет" в cp1251.
	body := []byte{0xCF, 0xF0, 0xE8, 0xE2, 0xE5, 0xF2}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `text/html; charset="Windows-1251"`)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	page, err := loopbackClient(0).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch упал: %v", err)
	}
	if !strings.Contains(page.Text, "Привет") {
		t.Fatalf("charset=Windows-1251 (в кавычках, верхний регистр) обязан декодироваться (блок 18.9): %q", page.Text)
	}
}

// TestDecodeWindows1251CyrillicBlock — таблица cp1251: кириллический блок
// 0xC0–0xFF непрерывен (А-я = U+0410–U+044F), specials на месте, не
// назначенный 0x98 — U+FFFD. Проверяется против канонической раскладки, не
// против самой таблицы реализации.
func TestDecodeWindows1251CyrillicBlock(t *testing.T) {
	var raw []byte
	for b := 0xC0; b <= 0xFF; b++ {
		raw = append(raw, byte(b))
	}
	got := []rune(decodeWindows1251(raw))
	if len(got) != 0x100-0xC0 {
		t.Fatalf("декодер обязан выдавать по руне на байт, вышло %d", len(got))
	}
	for i, r := range got {
		if want := rune(0x0410 + i); r != want {
			t.Fatalf("байт 0x%X: руна U+%04X, хочу U+%04X (кириллический блок cp1251 непрерывен)",
				0xC0+i, r, want)
		}
	}

	specials := map[byte]rune{
		0x80: 0x0402, // Ђ
		0x88: 0x20AC, // €
		0x98: 0xFFFD, // не назначен в cp1251
		0xA5: 0x20BD, // ₽
		0xA8: 0x0401, // Ё
		0xB8: 0x0451, // ё
		0xB9: 0x2116, // №
	}
	for b, want := range specials {
		if got := []rune(decodeWindows1251([]byte{b}))[0]; got != want {
			t.Errorf("байт 0x%X: руна U+%04X, хочу U+%04X", b, got, want)
		}
	}

	// ASCII проходит как есть.
	if got := decodeWindows1251([]byte("A z 0")); got != "A z 0" {
		t.Errorf("ASCII обязан проходить без изменений: %q", got)
	}
}

// TestFetchUtf8BOMOnlyBody — тело из одного BOM: после снятия остаётся пустой
// текст, без мусора в выдаче (блок 18.9).
func TestFetchUtf8BOMOnlyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	}))
	defer srv.Close()

	page, err := loopbackClient(0).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch упал: %v", err)
	}
	if page.Text != "" {
		t.Fatalf("тело из одного BOM обязано давать пустой текст, получено: %q", page.Text)
	}
}

// TestFetchEmptyContentTypeWithoutBody — пустой Content-Type отклоняется даже
// когда тело пустое: allowlist §15 проверяется до чтения тела (блок 18.9).
func TestFetchEmptyContentTypeWithoutBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "")
		fmt.Fprint(w, "")
	}))
	defer srv.Close()

	if _, err := loopbackClient(0).Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("пустой content-type с пустым телом должен отклоняться (блок 18.9)")
	}
}
