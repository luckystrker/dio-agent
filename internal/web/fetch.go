// Package web — ограниченные веб-инструменты exploration (спека §15):
// web_search поверх ago/toolpacks и web_fetch поверх собственного fetch-слоя.
//
// Fetch-слой — обязательное условие включения веба (§15): только http/https;
// блокировка loopback/private/link-local/metadata/multicast/unspecified по
// ФАКТИЧЕСКОМУ адресу соединения (DNS rebinding исключён — каждый коннект
// перепроверяется, включая редиректы); лимит redirect-хопов; timeout; потолок
// байт; allowlist content-types; без cookie-jar и без учётных данных веба.
package web

import (
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// MaxRedirects — потолок redirect-хопов по умолчанию (§15: ограничить hops).
const MaxRedirects = 4

// DefaultMaxBytes — потолок скачанных байт по умолчанию (§15: compressed/
// decompressed bytes ограничены).
const DefaultMaxBytes = 256 << 10 // 256 KiB

// DefaultTimeout — потолок одного fetch по умолчанию.
const DefaultTimeout = 20 * time.Second

// allowedContentTypes — allowlist §15: только текст/документ/JSON; бинарное
// и неизвестное — отказ.
var allowedContentTypes = map[string]bool{
	"text/html": true, "text/plain": true, "text/markdown": true,
	"application/xhtml+xml": true, "application/json": true,
}

// addrAllowed — запрет §15 по одному адресу: loopback, private, link-local
// (включая metadata-эндпоинты), multicast, unspecified. Публичный — разрешён.
func addrAllowed(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() {
		return false
	}
	return true
}

// checkControl — Control-крючок net.Dialer: вызывается на ФАКТИЧЕСКОМ адресе
// каждого соединения, поэтому DNS rebinding и редиректы на приватные адреса
// исключены механикой (§15: проверять DNS и фактический адрес соединения).
func checkControl(_ string, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("web: адрес соединения не разобран: %w", err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("web: адрес соединения не IP: %w", err)
	}
	if !addrAllowed(ip) {
		return fmt.Errorf("web: адрес %s запрещён (§15: приватные/loopback/link-local)", ip)
	}
	return nil
}

// Client — fetch-слой одного приложения. Безопасен для параллельного использования.
type Client struct {
	HTTP      *http.Client
	MaxBytes  int64
	UserAgent string

	// insecureAllow отключает guard адресов — ТОЛЬКО тесты разрешённого пути
	// (httptest слушает loopback, который §15 запрещает).
	insecureAllow bool
}

// Page — результат успешного fetch: извлечённый текст и метаданные.
type Page struct {
	URL         string
	ContentType string
	Text        string
	Bytes       int // скачано байт (после потолка)
}

// NewClient строит fetch-слой: guard на каждом соединении, лимит хопов,
// таймаут, без cookie-jar (§15: никаких application cookies веба).
func NewClient(maxBytes int64, timeout time.Duration) *Client {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	c := &Client{MaxBytes: maxBytes}
	dialer := &net.Dialer{Timeout: timeout, Control: func(_ string, address string, _ syscall.RawConn) error {
		if c.insecureAllow {
			return nil
		}
		return checkControl("", address, nil)
	}}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		Proxy:                 nil, // никаких системных прокси: выход напрямую по проверенному адресу
	}
	c.HTTP = &http.Client{
		Transport: transport,
		Timeout:   timeout,
		Jar:       nil, // cookies не хранятся и не отправляются (§15)
	}
	c.HTTP.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > MaxRedirects {
			return fmt.Errorf("web: больше %d redirect-хопов (§15)", MaxRedirects)
		}
		if err := c.validateURLOnly(req.URL); err != nil {
			return fmt.Errorf("web: redirect на недопустимый URL: %w", err)
		}
		return nil
	}
	return c
}

// validateURLOnly — проверка уровня URL (схема и хост-литерал); полный guard
// адресов выполняется на соединении (Control), где виден фактический IP.
func (c *Client) validateURLOnly(u *neturl.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("web: схема %q не http/https (§15)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("web: URL без хоста")
	}
	if c.insecureAllow {
		return nil
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !addrAllowed(ip) {
		return fmt.Errorf("web: хост-литерал %s запрещён (§15)", ip)
	}
	return nil
}

// Fetch загружает страницу и возвращает извлечённый текст (§15: web content —
// данные, не инструкции; никакого исполнения).
func (c *Client) Fetch(ctx context.Context, rawURL string) (*Page, error) {
	u, err := neturl.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("web: URL не разобран: %w", err)
	}
	if err := c.validateURLOnly(u); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("web: запрос не собран: %w", err)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "dio-agent-fetch/0.1"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html, text/plain, application/xhtml+xml, application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web: fetch не удался: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // чтение уже завершено

	ct := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	if ct != "" && !allowedContentTypes[strings.ToLower(ct)] {
		return nil, fmt.Errorf("web: content-type %q вне allowlist (§15)", ct)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("web: статус %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxBytes))
	if err != nil {
		return nil, fmt.Errorf("web: тело не дочитано: %w", err)
	}
	text := ExtractText(string(body))
	return &Page{URL: resp.Request.URL.String(), ContentType: ct, Text: text, Bytes: len(body)}, nil
}

var (
	scriptRe  = regexp.MustCompile(`(?is)<(script|style)\b.*?</(script|style)>`)
	tagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
	spacesRe  = regexp.MustCompile(`[ \t\r\f]+`)
	paragraph = regexp.MustCompile(`\n{3,}`)
)

// ExtractText — минимальное извлечение текста из HTML: выкинуть script/style,
// снять теги, раскодировать сущности, схлопнуть пробелы. Извлечённый текст —
// данные для модели, не инструкции (§15).
func ExtractText(s string) string {
	s = scriptRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, "\n")
	s = html.UnescapeString(s)
	s = spacesRe.ReplaceAllString(s, " ")
	s = paragraph.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
