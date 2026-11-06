// Веб в разговорном ходе (этап 36.1) и картинка хода (этап 36.2).
//
// Веб-инструменты — тот же fetch-слой §15 и те же поисковые инструменты, что у
// explore фона, но выдаются разговорному ходу по решению харнесса: только если
// веб включён и суточный лимит поисков разговорного пула не исчерпан. Модель
// решает, идти ли в веб; лимиты (2 поиска + 2 страницы за ход, общий потолок
// обращений к инструментам 5) — страховка. С 38.4 у каждого вида инструментов
// свой потолок: память ≤2, поиск ≤2, страницы ≤2 — иначе память съедает общий
// лимит раньше, чем модель дойдёт до ответа.
//
// Картинка — одна за ход: модель называет URL, runtime принимает его ТОЛЬКО
// если адрес встретился в результатах инструментов этого хода, качает через
// fetch-слой (image/*, потолок байт) и отдаёт отдельным пузырём после сообщений.
package character

import (
	"context"
	"log"
	"regexp"
	"strings"
	"sync"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agomiddleware "github.com/luckystrker/ago/middleware"

	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/web"
)

// Лимиты веба разговорного хода (36.1, §15/§16).
const (
	// WebSearchesPerTurn / WebFetchesPerTurn — потолок на один ход.
	WebSearchesPerTurn = 2
	WebFetchesPerTurn  = 2
	// WebSearchDailyLimit — сколько поисков разговорного пула бывает за сутки
	// (UTC); фон считает свои поиски отдельно (WEB_MAX_SEARCH_PER_TASK).
	WebSearchDailyLimit = 15
	// conversationToolsCap — общий потолок обращений к инструментам за ход,
	// когда выданы и память (2), и веб (2+2): шесть вызовов — это уже не ответ,
	// а исследование, которое разрешено фону.
	conversationToolsCap = 5
	// maxImageURLLen — потолок URL картинки: длиннее — не адрес, а мусор.
	maxImageURLLen = 2048

	// webFetchToolName — имя fetch-инструмента (совпадает с web.Tools).
	webFetchToolName = "web_fetch"
)

// webToolState решает, выдавать ли веб-инструменты на ходе: веб выключен или
// счётчик не прочитан — нет; суточный лимит выбран — нет, но панель честно
// скажет «поиск на сегодня исчерпан». Чистая функция (тестируется без БД).
func webToolState(enabled bool, used, limit int64, readErr error) (available, exhausted bool) {
	if !enabled || readErr != nil {
		return false, false
	}
	if limit <= 0 {
		limit = WebSearchDailyLimit
	}
	if used >= limit {
		return false, true
	}
	return true, false
}

// conversationToolLimit — общий потолок обращений к инструментам: память (2)
// и веб (2+2) вместе не больше conversationToolsCap, конфигурация модели может
// только снизить потолок, но не поднять его.
func conversationToolLimit(memoryEnabled bool, memoryLimit int, webEnabled bool, configured int) int {
	if !webEnabled {
		if !memoryEnabled {
			return 0
		}
		return memoryLimit
	}
	limit := WebSearchesPerTurn + WebFetchesPerTurn
	if memoryEnabled {
		limit += memoryLimit
	}
	if limit > conversationToolsCap {
		limit = conversationToolsCap
	}
	if configured > 0 && configured < limit {
		limit = configured
	}
	return limit
}

// Виды инструментов разговорного хода (38.4): у каждого свой потолок, чтобы
// память не съедала общий лимит на ходе с вебом (живой ход 238: пять вызовов
// памяти из пяти возможных).
const (
	toolKindMemory = "memory"
	toolKindSearch = "search"
	toolKindFetch  = "fetch"
)

// conversationKindCaps — раздельные потолки видов на ход: память ≤2, поиск ≤2,
// страницы ≤2 (38.4). Конфигурация может только снизить потолок памяти;
// выше двойки он не поднимается никогда.
func conversationKindCaps(memoryEnabled bool, memoryLimit int, webEnabled bool, searchName string) []ToolKindCap {
	var caps []ToolKindCap
	if memoryEnabled {
		limit := memoryLimit
		if limit <= 0 || limit > memoryToolsLimit {
			limit = memoryToolsLimit
		}
		caps = append(caps, ToolKindCap{
			Kind:   toolKindMemory,
			Limit:  limit,
			Names:  memoryToolNames(),
			Reject: "лимит обращений к памяти на ход исчерпан — отвечай тем, что уже есть",
		})
	}
	if webEnabled {
		if searchName != "" {
			caps = append(caps, ToolKindCap{
				Kind:   toolKindSearch,
				Limit:  WebSearchesPerTurn,
				Names:  []string{searchName},
				Reject: "лимит поиска на ход исчерпан — отвечай тем, что уже есть, или без поиска",
			})
		}
		caps = append(caps, ToolKindCap{
			Kind:   toolKindFetch,
			Limit:  WebFetchesPerTurn,
			Names:  []string{webFetchToolName},
			Reject: "лимит страниц на ход исчерпан — отвечай тем, что уже есть",
		})
	}
	return caps
}

// memoryToolNames — имена инструментов памяти (потолок вида, 38.4).
func memoryToolNames() []string {
	return []string{memory.ToolSearch, memory.ToolFindEntities, memory.ToolEntityCard}
}

// ImageFetcher — скачивание картинки через fetch-слой §15 (SSRF-проверки,
// allowlist image/*, потолок байт). Реализуется *web.Client; nil — картинки
// не отправляются.
type ImageFetcher interface {
	FetchImage(ctx context.Context, rawURL string) (*web.Image, error)
}

// webURLCollector собирает адреса, встретившиеся в результатах веб-инструментов
// ЭТОГО хода. Картинка вне множества не отправляется: модель не может принести
// URL из головы (§9.2 — данные извне только через проверенный канал).
type webURLCollector struct {
	mu   sync.Mutex
	urls map[string]struct{}
	alts map[string]string
}

func newWebURLCollector() *webURLCollector {
	return &webURLCollector{urls: make(map[string]struct{}), alts: make(map[string]string)}
}

func (c *webURLCollector) add(text string) {
	urls := collectToolResultURLs(text)
	alts := collectToolResultURLAlts(text)
	c.mu.Lock()
	for _, u := range urls {
		c.urls[normalizeCandidateURL(u)] = struct{}{}
	}
	for u, alt := range alts {
		if _, ok := c.alts[u]; !ok {
			c.alts[u] = alt
		}
	}
	c.mu.Unlock()
}

// altFor — подпись картинки из выдачи хода (alt со страницы); пусто — нет.
func (c *webURLCollector) altFor(rawURL string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.alts[normalizeCandidateURL(rawURL)]
}

// all — множество адресов, встретившихся в выдаче хода (для проверки image_url).
func (c *webURLCollector) all() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]bool, len(c.urls))
	for u := range c.urls {
		out[u] = true
	}
	return out
}

// urlCandidateRe — грубое извлечение http(s)-адресов из текста результата
// инструмента: поиск отдаёт JSON со ссылками, web_fetch — текст и список
// картинок. Точную проверку (SSRF, content-type) делает fetch-слой.
var urlCandidateRe = regexp.MustCompile(`https?://[^\s"'<>\[\]{}]+`)

// collectToolResultURLAlts — подписи картинок из строк выдачи «URL — alt»
// (web_fetch, этап 37.1): адрес и текст после него до конца строки.
func collectToolResultURLAlts(text string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		loc := urlCandidateRe.FindStringIndex(line)
		if loc == nil {
			continue
		}
		raw := line[loc[0]:loc[1]]
		u := normalizeCandidateURL(raw)
		if u == "" || u != raw {
			// Хвостовая пунктуация уже срезана — подпись начинается сразу
			// после адреса в исходной строке.
			raw = u
		}
		if raw == "" {
			continue
		}
		tail := strings.TrimSpace(line[loc[1]:])
		if !strings.HasPrefix(tail, "—") {
			// Подпись идёт только в формате списка web_fetch «URL — текст»;
			// произвольный хвост строки поиска подписью не считается.
			continue
		}
		tail = strings.TrimSpace(strings.TrimPrefix(tail, "—"))
		if tail == "" {
			continue
		}
		out[normalizeCandidateURL(raw)] = tail
	}
	return out
}

// collectToolResultURLs — адреса из результата инструмента без хвостовой
// пунктуации (точка в конце предложения не часть URL).
func collectToolResultURLs(text string) []string {
	raw := urlCandidateRe.FindAllString(text, -1)
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, u := range raw {
		u = normalizeCandidateURL(u)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// normalizeCandidateURL срезает обрамляющую пунктуацию, которая прилипает к
// адресу в тексте: «https://a/b.jpg.», «(https://a/b.jpg)», «https://a/b.jpg,»
// — но не трогает одиночную закрывающую скобку, если она открывается в адресе.
func normalizeCandidateURL(raw string) string {
	u := raw
	for len(u) > 0 {
		last := u[len(u)-1]
		if last == '.' || last == ',' || last == ';' || last == ':' || last == '!' ||
			last == '?' || last == '"' || last == '\'' || last == '»' {
			u = u[:len(u)-1]
			continue
		}
		if last == ')' && !hasUnmatchedOpenParen(u) {
			u = u[:len(u)-1]
			continue
		}
		if last == ']' && !hasUnmatchedOpenBracket(u) {
			u = u[:len(u)-1]
			continue
		}
		break
	}
	return u
}

func hasUnmatchedOpenParen(u string) bool {
	open, closed := 0, 0
	for _, r := range u {
		switch r {
		case '(':
			open++
		case ')':
			closed++
		}
	}
	return open > closed
}

func hasUnmatchedOpenBracket(u string) bool {
	open, closed := 0, 0
	for _, r := range u {
		switch r {
		case '[':
			open++
		case ']':
			closed++
		}
	}
	return open > closed
}

// webResultMW — middleware журналирования адресов выдачи: после исполнения
// веб-инструмента собирает URL из текста результата (36.2). Сбои инструмента
// ничего не добавляют.
type webResultMW struct {
	agomiddleware.Base
	collector  *webURLCollector
	searchName string
}

func (m *webResultMW) OnToolExecution(next agomiddleware.ToolExecFunc, ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	result, err := next(ev, cx)
	if err == nil && (ev.Name == webFetchToolName || ev.Name == m.searchName) {
		if text := toolResultText(result); text != "" {
			m.collector.add(text)
		}
	}
	return result, err
}

// toolResultText — текстовые части ToolResult (модель видит их же).
func toolResultText(result events.Event) string {
	out, ok := events.AsValue(result).(events.ToolResultEvent)
	if !ok {
		return ""
	}
	var text string
	for _, p := range out.Result.Parts {
		if part, ok := events.AsValue(p).(events.TextInput); ok {
			text += part.Content + "\n"
		}
	}
	return text
}

// webUsageMW — суточный учёт поисков разговорного пула (36.1). Счётчик
// увеличивается до исполнения; исчерпание лимита внутри хода (remaining
// досчитан при сборке контекста) отвечает модели отказом, а не сетевым вызовом.
type webUsageMW struct {
	agomiddleware.Base
	name   string
	db     web.UsageDB
	logger *log.Logger

	mu        sync.Mutex
	remaining int64
}

func (m *webUsageMW) OnToolExecution(next agomiddleware.ToolExecFunc, ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	if ev.Name != m.name {
		return next(ev, cx)
	}
	m.mu.Lock()
	m.remaining--
	over := m.remaining < 0
	m.mu.Unlock()
	if over {
		if m.logger != nil {
			m.logger.Printf("веб: суточный лимит поисков исчерпан — вызов %q отклонён (36.1)", ev.Name)
		}
		return &events.ToolErrorEvent{
			ToolResultEvent: events.ToolResultEvent{ParentID: ev.ID, Name: ev.Name},
			Error:           "поиск на сегодня исчерпан — отвечай тем, что уже есть, или без поиска",
		}, nil
	}
	if err := (web.UsageStore{}).IncrementSearch(context.Background(), m.db); err != nil && m.logger != nil {
		m.logger.Printf("веб: поиск не учтён в суточном счётчике: %v", err)
	}
	return next(ev, cx)
}

// resolveImage — проверка и скачивание картинки хода (36.2): URL обязан быть в
// выдаче этого хода (allowed nil — восстановление сохранённого model_output,
// проверка была сделана при генерации), скачивание — fetch-слоем §15. Любая
// проблема мягко обнуляет картинку: ход живёт, в лог — причина.
func (r *Runner) resolveImage(ctx context.Context, turnID int64, rawURL, alt string, allowed map[string]bool) *OutboxImage {
	u := normalizeCandidateURL(strings.TrimSpace(rawURL))
	if u == "" {
		return nil
	}
	if allowed != nil && !allowed[u] {
		r.logf("ход %d: image_url %q не встречался в выдаче веб-инструментов этого хода — картинка не отправлена (36.2)", turnID, u)
		return nil
	}
	if r.ImageFetcher == nil {
		r.logf("ход %d: image_url %q есть, но fetch-слой картинок не настроен — картинка не отправлена", turnID, u)
		return nil
	}
	img, err := r.ImageFetcher.FetchImage(ctx, u)
	if err != nil {
		r.logf("ход %d: картинка %q не скачана — ход без картинки: %v", turnID, u, err)
		return nil
	}
	if img == nil || len(img.Bytes) == 0 {
		r.logf("ход %d: картинка %q пустая — ход без картинки", turnID, u)
		return nil
	}
	r.logf("ход %d: картинка к отправке: %s (%s, %d байт)", turnID, img.URL, img.ContentType, len(img.Bytes))
	alt = strings.TrimSpace(alt)
	return &OutboxImage{SourceURL: img.URL, ContentType: img.ContentType, Bytes: img.Bytes, Alt: alt}
}
