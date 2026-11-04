// Картинки веб-слоя (этап 36.2): ссылки на картинки страницы для image_url и
// скачивание картинки через тот же fetch-слой §15 — те же SSRF-проверки на
// фактическом адресе соединения, allowlist content-type image/*, потолок байт.
package web

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
)

// allowedImageTypes — что можно отправлять в чат картинкой (Telegram
// sendPhoto): jpeg/png/webp/gif; svg и прочее — не картинка для чата.
var allowedImageTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true,
}

// Image — скачанная картинка: байты и content-type после проверок §15.
type Image struct {
	URL         string
	ContentType string
	Bytes       []byte
}

var (
	metaTagRe = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	imgTagRe  = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	attrRe    = regexp.MustCompile(`(?is)([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// ExtractImageURLs — абсолютные http(s)-адреса картинок страницы: og:image /
// twitter:image, затем <img src>/<img data-src>. Относительные разрешаются от
// адреса страницы, data:/javascript:/ftp: отбрасываются, дубли схлопываются,
// список ограничен maxPageImages. Функция чистая — тестируется без сети.
func ExtractImageURLs(pageHTML, baseURL string) []string {
	base, err := neturl.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, maxPageImages)
	add := func(raw string) {
		if len(out) >= maxPageImages {
			return
		}
		raw = strings.TrimSpace(html.UnescapeString(raw))
		if raw == "" || strings.HasPrefix(strings.ToLower(raw), "data:") {
			return
		}
		u, err := neturl.Parse(raw)
		if err != nil {
			return
		}
		abs := base.ResolveReference(u)
		if abs.Host == "" || (abs.Scheme != "http" && abs.Scheme != "https") {
			return
		}
		abs.Fragment = ""
		key := abs.String()
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}

	for _, tag := range metaTagRe.FindAllString(pageHTML, -1) {
		attrs := parseTagAttrs(tag)
		kind := strings.ToLower(attrs["property"])
		if kind == "" {
			kind = strings.ToLower(attrs["name"])
		}
		switch kind {
		case "og:image", "og:image:url", "og:image:secure_url", "twitter:image":
			add(attrs["content"])
		}
	}
	for _, tag := range imgTagRe.FindAllString(pageHTML, -1) {
		attrs := parseTagAttrs(tag)
		src := attrs["src"]
		if strings.TrimSpace(src) == "" {
			src = attrs["data-src"]
		}
		add(src)
	}
	return out
}

// parseTagAttrs — атрибуты одного тега в нижнем регистре: значение в обычных
// или одинарных кавычках либо без них.
func parseTagAttrs(tag string) map[string]string {
	attrs := make(map[string]string, 8)
	for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
		name := strings.ToLower(m[1])
		value := m[3]
		if value == "" {
			value = m[4]
		}
		if value == "" {
			value = m[5]
		}
		if _, exists := attrs[name]; !exists {
			attrs[name] = value
		}
	}
	return attrs
}

// imageLimit — потолок байт картинки: поле клиента или MaxImageBytes.
func (c *Client) imageLimit() int64 {
	if c.ImageMaxBytes > 0 {
		return c.ImageMaxBytes
	}
	return MaxImageBytes
}

// FetchImage скачивает картинку по URL через fetch-слой §15:
//   - только http/https, guard фактического адреса соединения (редиректы и DNS
//     rebinding перепроверяются так же, как в Fetch);
//   - content-type только image/jpeg|png|webp|gif;
//   - потолок байт MaxImageBytes (поле ImageMaxBytes — тестовая настройка);
//   - статус 200, пустая картинка — ошибка.
//
// Ошибка — мягкая для хода: runtime просто не отправляет картинку.
func (c *Client) FetchImage(ctx context.Context, rawURL string) (*Image, error) {
	u, err := neturl.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("web: URL картинки не разобран: %w", err)
	}
	if err := c.validateURLOnly(u); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("web: запрос картинки не собран: %w", err)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "dio-agent-fetch/0.1"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "image/jpeg, image/png, image/webp, image/gif")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web: картинка не скачана: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // чтение завершено ниже

	mediaType, _ := parseContentType(resp.Header.Get("Content-Type"))
	if !allowedImageTypes[mediaType] {
		return nil, fmt.Errorf("web: content-type %q вне allowlist картинок (§15)", mediaType)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("web: статус картинки %d", resp.StatusCode)
	}
	limit := c.imageLimit()
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("web: картинка больше потолка %d байт", limit)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("web: картинка не дочитана: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("web: картинка больше потолка %d байт", limit)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("web: пустая картинка")
	}
	return &Image{URL: resp.Request.URL.String(), ContentType: mediaType, Bytes: body}, nil
}
