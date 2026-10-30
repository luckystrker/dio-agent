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
		{"93.184.216.34", true},                      // публичный
		{"2606:2800:220:1:248:1893:25c8:1946", true}, // публичный v6
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
