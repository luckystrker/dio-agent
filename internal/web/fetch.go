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
	"bytes"
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
	"unicode/utf8"
)

// MaxRedirects — потолок redirect-хопов по умолчанию (§15: ограничить hops).
const MaxRedirects = 4

// DefaultMaxBytes — потолок скачанных байт по умолчанию (§15: compressed/
// decompressed bytes ограничены).
const DefaultMaxBytes = 256 << 10 // 256 KiB

// DefaultTimeout — потолок одного fetch по умолчанию.
const DefaultTimeout = 20 * time.Second

// MaxTextRunes — потолок текста страницы в выдаче модели (блок 18.9): раньше
// до 256 КиБ текста уходило целиком, без обрезки и без пометки.
const MaxTextRunes = 10_000

// MaxImageBytes — потолок скачиваемой картинки (этап 36.2): картинка уходит в
// чат отдельным пузырём, больше 5 МиБ не тянем (Telegram всё равно сожмёт,
// а память процесса и диск БД не резиновые).
const MaxImageBytes = 5 << 20

// maxPageImages — сколько ссылок на картинки страницы отдаём модели: список —
// подсказка для image_url, а не каталог сайта.
const maxPageImages = 20

// truncationMarker — честная пометка обрезки в конце выдачи (блок 18.9).
const truncationMarker = "[страница обрезана]"

// allowedContentTypes — allowlist §15: только текст/документ/JSON; бинарное
// и неизвестное — отказ.
var allowedContentTypes = map[string]bool{
	"text/html": true, "text/plain": true, "text/markdown": true,
	"application/xhtml+xml": true, "application/json": true,
}

// forbiddenPrefixes — запрещённые префиксы сверх встроенных категорий: CGNAT
// 100.64.0.0/10 (переводная NAT-сеть провайдера — не публичный хост) и NAT64
// 64:ff9b::/96 (отображение IPv4 в IPv6; в него маскируются и приватные
// адреса) — блок 18.10: оба проходили проверки netip как «публичные».
var forbiddenPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64
	// minor 12: зарезервированные диапазоны, которые netip считает публичными.
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved + broadcast
	netip.MustParsePrefix("2002::/16"),     // 6to4
	netip.MustParsePrefix("2001::/32"),     // Teredo
}

// addrAllowed — запрет §15 по одному адресу: loopback, private, link-local
// (включая metadata-эндпоинты), multicast, unspecified, CGNAT, NAT64.
// Публичный — разрешён. IPv4-mapped форма (::ffff:a.b.c.d) нормализуется
// (Unmap) до проверок: netip.Prefix.Contains не матчит 4-in-6 адрес к
// v4-префиксу, и CGNAT в mapped форме обходил запрет (18.10, находка приёмки).
func addrAllowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() {
		return false
	}
	for _, p := range forbiddenPrefixes {
		if p.Contains(ip) {
			return false
		}
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
	// ImageMaxBytes — потолок скачиваемой картинки (этап 36.2); 0 —
	// MaxImageBytes. Отдельно от MaxBytes текстового fetch-слоя.
	ImageMaxBytes int64

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
	// Images — абсолютные http(s)-адреса картинок страницы (og:image, img src):
	// допустимые кандидаты для image_url хода (этап 36.2).
	Images []string
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

	mediaType, charset := parseContentType(resp.Header.Get("Content-Type"))
	if mediaType == "" {
		return nil, fmt.Errorf("web: пустой content-type вне allowlist (§15, блок 18.9)")
	}
	if !allowedContentTypes[mediaType] {
		return nil, fmt.Errorf("web: content-type %q вне allowlist (§15)", mediaType)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("web: статус %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxBytes))
	if err != nil {
		return nil, fmt.Errorf("web: тело не дочитано: %w", err)
	}
	pageHTML := decodeBody(body, charset)
	text := truncateMarked(ExtractText(pageHTML), MaxTextRunes)
	return &Page{
		URL:         resp.Request.URL.String(),
		ContentType: mediaType,
		Text:        text,
		Bytes:       len(body),
		Images:      ExtractImageURLs(pageHTML, resp.Request.URL.String()),
	}, nil
}

// parseContentType — медиа-тип (нижний регистр, без пробелов) и charset из
// полного заголовка Content-Type: "text/html; charset=windows-1251" →
// ("text/html", "windows-1251") (блок 18.9: charset раньше игнорировался).
func parseContentType(header string) (mediaType, charset string) {
	parts := strings.Split(header, ";")
	mediaType = strings.ToLower(strings.TrimSpace(parts[0]))
	for _, p := range parts[1:] {
		name, val, ok := strings.Cut(p, "=")
		if ok && strings.ToLower(strings.TrimSpace(name)) == "charset" {
			charset = strings.ToLower(strings.Trim(strings.TrimSpace(val), `"`))
		}
	}
	return mediaType, charset
}

// utf8BOM — маркер порядка байт UTF-8, снимается до извлечения текста.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// decodeBody — тело ответа в текст (блок 18.9): декодирование charset
// (cp1251 — мини-таблица внутри пакета), снятие UTF-8 BOM и подрезка обрыва
// многобайтной руны, оставшегося от байтового потолка (LimitReader режет
// посередине символа).
func decodeBody(body []byte, charset string) string {
	switch charset {
	case "windows-1251", "cp1251":
		return decodeWindows1251(body)
	default: // utf-8 и неуказанный — utf-8-совместимая обработка
		b := bytes.TrimPrefix(body, utf8BOM)
		return string(trimBrokenRuneTail(b))
	}
}

// trimBrokenRuneTail отрезает незавершённую многобайтную последовательность
// в конце байт. Честный U+FFFD (три байта) не трогается: битым хвостом считается
// только одинокий некодируемый байт.
func trimBrokenRuneTail(b []byte) []byte {
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size != 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

// truncateMarked — обрезка до limit рун по границе руны с честным маркером
// обрезки в конце (блок 18.9).
func truncateMarked(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + truncationMarker
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
