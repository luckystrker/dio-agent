package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestAddrAllowedTable — таблица запретов §15 по типам адресов.
func TestAddrAllowedTable(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", false},                         // loopback
		{"10.0.0.5", false},                          // private
		{"192.168.1.1", false},                       // private
		{"172.16.0.9", false},                        // private
		{"169.254.169.254", false},                   // link-local (metadata-эндпоинт)
		{"fe80::1", false},                           // link-local v6
		{"::1", false},                               // loopback v6
		{"0.0.0.0", false},                           // unspecified
		{"224.0.0.1", false},                         // multicast
		{"ff02::1", false},                           // multicast v6
		{"100.64.0.1", false},                        // CGNAT 100.64.0.0/10 (18.10)
		{"100.127.255.254", false},                   // CGNAT, верх диапазона (18.10)
		{"64:ff9b::1", false},                        // NAT64 64:ff9b::/96 (18.10)
		{"64:ff9b::192.0.2.33", false},               // NAT64-отображение IPv4 (18.10)
		{"198.18.0.1", false},                        // benchmarking 198.18.0.0/15 (minor 12)
		{"198.19.255.255", false},                    // benchmarking, верх диапазона (minor 12)
		{"192.0.0.8", false},                         // IETF assignments 192.0.0.0/24 (minor 12)
		{"240.0.0.1", false},                         // reserved 240.0.0.0/4 (minor 12)
		{"255.255.255.255", false},                   // broadcast в 240.0.0.0/4 (minor 12)
		{"2002::1", false},                           // 6to4 2002::/16 (minor 12)
		{"2001::1", false},                           // Teredo 2001::/32 (minor 12)
		{"198.20.0.1", true},                         // публичный после benchmarking (minor 12)
		{"100.63.255.255", true},                     // публичный до CGNAT (18.10)
		{"100.128.0.0", true},                        // публичный после CGNAT (18.10)
		{"93.184.216.34", true},                      // публичный
		{"2606:2800:220:1:248:1893:25c8:1946", true}, // публичный v6
		{"2001:db8::1", true},                        // публичный v6 вне NAT64 (18.10)
	}
	for _, tc := range cases {
		if got := addrAllowed(netip.MustParseAddr(tc.ip)); got != tc.want {
			t.Errorf("%s: addrAllowed=%v, хочу %v", tc.ip, got, tc.want)
		}
	}
}

// loopbackClient — клиент с отключённым guard (httptest слушает loopback,
// который §15 запрещает): проверяет разрешённый путь, не адресу.
func loopbackClient(maxBytes int64) *Client {
	c := NewClient(maxBytes, 2*time.Second)
	c.insecureAllow = true
	return c
}

func TestFetchBlocksLoopbackByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<p>приватно?</p>"))
	}))
	defer srv.Close()

	c := NewClient(0, 2*time.Second)
	if c.insecureAllow {
		t.Fatal("клиент по умолчанию не должен отключать guard")
	}
	_, err := c.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "запрещён") {
		t.Fatalf("loopback должен блокироваться §15, получено: %v", err)
	}
}

func TestFetchAllowedPathExtractsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head><style>b{color:red}</style></head>" +
			"<body><h1>Заголовок</h1><script>alert(1)</script><p>Текст &amp; сущности</p></body></html>"))
	}))
	defer srv.Close()

	page, err := loopbackClient(0).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("разрешённый fetch упал: %v", err)
	}
	for _, want := range []string{"Заголовок", "Текст & сущности"} {
		if !strings.Contains(page.Text, want) {
			t.Fatalf("текст без %q: %q", want, page.Text)
		}
	}
	if strings.Contains(page.Text, "alert") || strings.Contains(page.Text, "color:red") {
		t.Fatalf("script/style протекли в текст: %q", page.Text)
	}
}

func TestFetchRejectsNonHTTPScheme(t *testing.T) {
	if _, err := loopbackClient(0).Fetch(context.Background(), "ftp://example.com/x"); err == nil {
		t.Fatal("ftp должен быть отклонён (§15: только http/https)")
	}
}

func TestFetchRejectsDisallowedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0, 1, 2, 3})
	}))
	defer srv.Close()
	if _, err := loopbackClient(0).Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("бинарный content-type должен отклоняться (§15 allowlist)")
	}
}

func TestFetchSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("а", 10_000)))
	}))
	defer srv.Close()
	page, err := loopbackClient(1000).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch упал: %v", err)
	}
	if page.Bytes != 1000 {
		t.Fatalf("скачано %d байт, хочу потолок 1000", page.Bytes)
	}
}

func TestFetchRedirectHopLimit(t *testing.T) {
	// Цепочка /hop0 → /hop1 → …: больше MaxRedirects подряд — ошибка §15.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for i := range 8 {
		i := i
		mux.HandleFunc(fmt.Sprintf("/hop%d", i), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, fmt.Sprintf("%s/hop%d", srv.URL, i+1), http.StatusFound)
		})
	}

	_, err := loopbackClient(0).Fetch(context.Background(), srv.URL+"/hop0")
	if err == nil || !strings.Contains(err.Error(), "redirect-хопов") {
		t.Fatalf("хочу отказ по лимиту хопов, получено: %v", err)
	}
}

func TestFetchNoCookiesSent(t *testing.T) {
	var sawCookie bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			sawCookie = true
		}
		w.Header().Set("Set-Cookie", "tracking=1; Path=/")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := loopbackClient(0)
	for range 3 { // сервер ставит cookie — клиент не должен её накапливать и возвращать
		if _, err := c.Fetch(context.Background(), srv.URL); err != nil {
			t.Fatalf("fetch упал: %v", err)
		}
	}
	if sawCookie {
		t.Fatal("cookie веба вернулись в запрос — §15 запрещает application cookies")
	}
	if c.HTTP.Jar != nil {
		t.Fatal("cookie-jar должен отсутствовать (§15)")
	}
}

// TestFetchTruncatesLongText — до ~10К рун по границе руны с честным маркером
// «[страница обрезана]» (блок 18.9): раньше до 256 КиБ текста уходило модели
// целиком без обрезки и без пометки.
func TestFetchTruncatesLongText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(strings.Repeat("слово ", 6000))) // 30К рун
	}))
	defer srv.Close()

	page, err := loopbackClient(0).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch упал: %v", err)
	}
	if !strings.HasSuffix(page.Text, "[страница обрезана]") {
		t.Fatalf("обрезанный текст обязан заканчиваться маркером «[страница обрезана]» (блок 18.9), хвост: %q",
			tail(page.Text, 40))
	}
	if got := len([]rune(page.Text)); got > 10000+len([]rune("[страница обрезана]")) {
		t.Fatalf("текст обязан обрезаться до ~10К рун, в выдаче %d рун", got)
	}
}

// TestFetchRejectsEmptyContentType — пустой Content-Type не входит в allowlist
// §15: отказ инструмента (блок 18.9; раньше проходил насквозь).
func TestFetchRejectsEmptyContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "")
		_, _ = w.Write([]byte("тишина"))
	}))
	defer srv.Close()

	if _, err := loopbackClient(0).Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("пустой content-type должен отклоняться (allowlist §15, блок 18.9)")
	}
}

// TestFetchDecodesWindows1251 — charset=windows-1251 декодируется мини-таблицей
// внутри internal/web (блок 18.9; раньше cp1251 уходил в модель как U+FFFD-мусор).
// Байты — каноническое cp1251-кодирование строки, независимо от таблицы реализации.
func TestFetchDecodesWindows1251(t *testing.T) {
	// "Привет, Ёлка — «тесто»" в cp1251: А-я = 0xC0-0xFF, Ё=0xA8, ё=0xB8,
	// — = 0x97, « = 0xAB, » = 0xBB.
	body := []byte{
		0xCF, 0xF0, 0xE8, 0xE2, 0xE5, 0xF2, 0x2C, 0x20, 0xA8, 0xEB, 0xEA, 0xE0,
		0x20, 0x97, 0x20, 0xAB, 0xF2, 0xE5, 0xF1, 0xF2, 0xEE, 0xBB,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	page, err := loopbackClient(0).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch упал: %v", err)
	}
	want := "Привет, Ёлка — «тесто»"
	if !strings.Contains(page.Text, want) {
		t.Fatalf("cp1251 обязан декодироваться (блок 18.9): хочу %q, получено %q", want, page.Text)
	}
}

// TestFetchStripsUTF8BOM — UTF-8 BOM снимается до извлечения текста (блок 18.9).
func TestFetchStripsUTF8BOM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("\xEF\xBB\xBFтекст с BOM"))
	}))
	defer srv.Close()

	page, err := loopbackClient(0).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch упал: %v", err)
	}
	if strings.Contains(page.Text, "\uFEFF") {
		t.Fatalf("BOM обязан сниматься (блок 18.9), текст: %q", page.Text)
	}
	if !strings.Contains(page.Text, "текст с BOM") {
		t.Fatalf("текст после BOM обязан сохраниться, получено: %q", page.Text)
	}
}

// TestFetchByteCapCutsRuneBoundary — байтовый потолок режет многобайтный UTF-8
// посередине символа: обрыванный хвост подрезается по границе руны, в выдаче
// не остаётся битых байт (блок 18.9).
func TestFetchByteCapCutsRuneBoundary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("а", 10_000))) // «а» = 2 байта в UTF-8
	}))
	defer srv.Close()

	// 1001 байт = 500 полных рун + 1 байт-огрызок многобайтной последовательности.
	page, err := loopbackClient(1001).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch упал: %v", err)
	}
	if !utf8.ValidString(page.Text) {
		t.Fatalf("битый хвост руны обязан подрезаться по границе руны (блок 18.9), текст невалиден: %q",
			tail(page.Text, 10))
	}
	if got := len([]rune(page.Text)); got != 500 {
		t.Fatalf("ожидаю 500 целых рун под потолком байт, получено %d", got)
	}
}

// tail — последние n рун строки (диагностика в сообщениях об ошибке).
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}
