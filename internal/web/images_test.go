// Тесты картинок веб-слоя (этап 36.2): извлечение ссылок на картинки из
// страницы (og:image + img src, относительные → абсолютные), скачивание
// картинки через тот же fetch-слой §15 (content-type, потолок байт, SSRF).
package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExtractImageURLs(t *testing.T) {
	page := `<html><head>
		<meta property="og:image" content="/img/cover.png">
		<meta name="twitter:image" content="https://cdn.example.com/tw.jpg">
	</head><body>
		<img src="https://cdn.example.com/a.jpg">
		<img src="data:image/png;base64,AAAA">
		<img src="//cdn.example.com/b.webp">
		<img src="c.gif">
		<img src="/img/cover.png">
	</body></html>`

	got := ExtractImageURLs(page, "https://example.com/news/1")
	want := []string{
		"https://example.com/img/cover.png",
		"https://cdn.example.com/tw.jpg",
		"https://cdn.example.com/a.jpg",
		"https://cdn.example.com/b.webp",
		"https://example.com/news/c.gif",
	}
	if len(got) != len(want) {
		t.Fatalf("ссылок на картинки %d (%v), хочу %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("картинка %d = %q, хочу %q (все: %v)", i, got[i], want[i], got)
		}
	}
}

func TestExtractImageURLsCapsAndIgnoresBad(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<html><body>`)
	for i := 0; i < maxPageImages+5; i++ {
		b.WriteString(`<img src="/i/` + fmt.Sprintf("%d", i) + `.jpg">`)
	}
	b.WriteString(`<img src="javascript:alert(1)"><img src=""><img src="ftp://example.com/x.png"></body></html>`)

	got := ExtractImageURLs(b.String(), "https://example.com/")
	if len(got) != maxPageImages {
		t.Fatalf("ссылок %d, хочу потолок %d", len(got), maxPageImages)
	}
	for _, u := range got {
		if !strings.HasPrefix(u, "https://example.com/i/") {
			t.Fatalf("в выдачу попал недопустимый адрес %q", u)
		}
	}
}

func TestFetchIncludesImageURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><meta property="og:image" content="/cover.png"></head>
			<body><p>текст</p><img src="/photo.jpg"></body></html>`))
	}))
	defer srv.Close()

	page, err := loopbackClient(0).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch не удался: %v", err)
	}
	if len(page.Images) != 2 || page.Images[0] != srv.URL+"/cover.png" || page.Images[1] != srv.URL+"/photo.jpg" {
		t.Fatalf("картинки страницы = %v, хочу [%s/cover.png %s/photo.jpg]", page.Images, srv.URL, srv.URL)
	}
}

func TestFetchImageAllowed(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n fake")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "image/png")
		_, _ = w.Write(png)
	}))
	defer srv.Close()

	img, err := loopbackClient(0).FetchImage(context.Background(), srv.URL+"/a.png")
	if err != nil {
		t.Fatalf("картинка не скачана: %v", err)
	}
	if img.ContentType != "image/png" || string(img.Bytes) != string(png) {
		t.Fatalf("картинка = %q (%d байт), хочу image/png и исходные байты", img.ContentType, len(img.Bytes))
	}
}

func TestFetchImageRejectsNonImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte("<html>это не картинка</html>"))
	}))
	defer srv.Close()

	if _, err := loopbackClient(0).FetchImage(context.Background(), srv.URL); err == nil ||
		!strings.Contains(err.Error(), "content-type") {
		t.Fatalf("не-image content-type должен отклоняться, получено: %v", err)
	}
}

func TestFetchImageRejectsOversize(t *testing.T) {
	c := loopbackClient(0)
	c.ImageMaxBytes = 16
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "image/jpeg")
		_, _ = w.Write(make([]byte, 1024))
	}))
	defer srv.Close()

	if _, err := c.FetchImage(context.Background(), srv.URL); err == nil ||
		!strings.Contains(err.Error(), "больше потолка") {
		t.Fatalf("слишком большая картинка должна отклоняться, получено: %v", err)
	}
}

func TestFetchImageRejectsNonHTTP(t *testing.T) {
	if _, err := NewClient(0, time.Second).FetchImage(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("не-http схема картинки должна отклоняться")
	}
}

func TestFetchImageBlocksLoopbackByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "image/png")
		_, _ = w.Write([]byte("png"))
	}))
	defer srv.Close()

	if _, err := NewClient(0, time.Second).FetchImage(context.Background(), srv.URL); err == nil ||
		!strings.Contains(err.Error(), "запрещён") {
		t.Fatalf("картинка с loopback должна блокироваться §15, получено: %v", err)
	}
}
