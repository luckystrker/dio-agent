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
	"encoding/json"
	"log"
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

// webURLCollector собирает адреса, встретившиеся в СТРУКТУРИРОВАННОЙ выдаче
// веб-инструментов ЭТОГО хода (41.2 M2): список картинок web_fetch
// (`web.ImagesBlockHeader`) и ссылки результатов поиска (href/url/images в
// JSON-выдаче). Адреса из свободного текста страницы не собираются: иначе
// страница (и промпт-инъекция из web_fetch/памяти) управляла бы множеством
// разрешённых image_url — проверка «URL из выдачи» аутентифицировала бы
// подстроку, а не предложение картинки.
type webURLCollector struct {
	mu   sync.Mutex
	urls map[string]struct{}
	alts map[string]string
}

func newWebURLCollector() *webURLCollector {
	return &webURLCollector{urls: make(map[string]struct{}), alts: make(map[string]string)}
}

// addStructured принимает готовые пары «адрес — подпись» из структурированного
// источника; пустые адреса отбрасываются.
func (c *webURLCollector) addStructured(pairs map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for raw, alt := range pairs {
		u := normalizeCandidateURL(strings.TrimSpace(raw))
		if u == "" {
			continue
		}
		c.urls[u] = struct{}{}
		if alt = sanitizeInlineNote(alt); alt != "" {
			if _, ok := c.alts[u]; !ok {
				c.alts[u] = alt
			}
		}
	}
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

// collectPageImageURLs — картинки из структурированного парта web_fetch:
// список [{url, alt}]. Свободный текст результата не разбирается (41.2 M2):
// страница не управляет множеством разрешённых image_url.
func collectPageImageURLs(data any) map[string]string {
	m := normalizeJSONData(data)
	if m == nil {
		return nil
	}
	items, _ := m["images"].([]any)
	out := make(map[string]string, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case string:
			out[v] = ""
		case map[string]any:
			u, _ := v["url"].(string)
			if u == "" {
				continue
			}
			alt, _ := v["alt"].(string)
			out[u] = alt
		}
	}
	return out
}

// normalizeJSONData приводит значение выдачи к map[string]any через JSON:
// инструменты ago кладут в DataInput Go-структуры (toolpacks.*SearchResponse),
// а не map — обход по полям должен видеть одинаковую форму (41.2 M2).
func normalizeJSONData(data any) map[string]any {
	if m, ok := data.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// searchResultFieldKeys — поля JSON-выдачи поиска, которые являются адресами
// (а не свободным текстом: title/body/content/snippet/answer адресами не
// считаются — 41.2 M2).
var searchResultFieldKeys = map[string]bool{
	"href": true, "url": true, "image_url": true, "images": true,
}

// collectSearchResultURLs — адреса из структурированной JSON-выдачи поиска:
// поля href/url/image_url/images на любой глубине. Свободный текст (body,
// snippet, answer) не разбирается.
func collectSearchResultURLs(data any) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch val := v.(type) {
		case map[string]any:
			for key, item := range val {
				if searchResultFieldKeys[strings.ToLower(key)] {
					out = append(out, stringURLs(item)...)
					continue
				}
				walk(item)
			}
		case []any:
			for _, item := range val {
				walk(item)
			}
		}
	}
	walk(normalizeJSONData(data))
	return out
}

// stringURLs вытаскивает адреса из значения поля выдачи (строка или список строк).
func stringURLs(v any) []string {
	switch val := v.(type) {
	case string:
		if u := normalizeCandidateURL(strings.TrimSpace(val)); u != "" && strings.HasPrefix(u, "http") {
			return []string{u}
		}
	case []any:
		var out []string
		for _, item := range val {
			out = append(out, stringURLs(item)...)
		}
		return out
	}
	return nil
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

// webResultMW — middleware сбора СТРУКТУРИРОВАННЫХ адресов выдачи (36.2,
// 41.2 M2): у web_fetch — список картинок страницы (с подписями), у поиска —
// поля JSON-выдачи href/url/images. Свободный текст результата адресов не даёт;
// сбои инструмента ничего не добавляют.
type webResultMW struct {
	agomiddleware.Base
	collector  *webURLCollector
	searchName string
}

func (m *webResultMW) OnToolExecution(next agomiddleware.ToolExecFunc, ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	result, err := next(ev, cx)
	if err != nil {
		return result, err
	}
	out, ok := events.AsValue(result).(events.ToolResultEvent)
	if !ok {
		return result, nil
	}
	switch ev.Name {
	case webFetchToolName:
		for _, p := range out.Result.Parts {
			if part, ok := events.AsValue(p).(events.DataInput); ok {
				if pairs := collectPageImageURLs(part.Data); len(pairs) > 0 {
					m.collector.addStructured(pairs)
				}
			}
		}
	case m.searchName:
		for _, p := range out.Result.Parts {
			if part, ok := events.AsValue(p).(events.DataInput); ok {
				pairs := make(map[string]string)
				for _, u := range collectSearchResultURLs(part.Data) {
					pairs[u] = ""
				}
				m.collector.addStructured(pairs)
			}
		}
	}
	return result, err
}

// webUsageMW — суточный учёт поисков разговорного пула (36.1). Счётчик
// увеличивается после УСПЕШНОГО исполнения (m16): исчерпание лимита внутри хода
// (remaining досчитан при сборке контекста) отвечает модели отказом, а не
// сетевым вызовом.
type webUsageMW struct {
	agomiddleware.Base
	name   string
	db     web.UsageDB
	logger *log.Logger
	// onExhausted — закрыть инструменты хода (tool_choice=none), когда
	// суточный лимит исчерпан прямо на ходе (39.1/39.2): следующий вызов
	// модели обязан дать текст, а определения остаются объявлены.
	onExhausted func()

	mu        sync.Mutex
	remaining int64
}

func (m *webUsageMW) OnToolExecution(next agomiddleware.ToolExecFunc, ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	if ev.Name != m.name {
		return next(ev, cx)
	}
	m.mu.Lock()
	over := m.remaining <= 0
	if !over {
		m.remaining-- // резерв: параллельные вызовы одного блока не переберут лимит
	}
	m.mu.Unlock()
	if over {
		if m.logger != nil {
			m.logger.Printf("веб: суточный лимит поисков исчерпан — вызов %q отклонён (36.1)", ev.Name)
		}
		if m.onExhausted != nil {
			m.onExhausted()
		}
		return &events.ToolErrorEvent{
			ToolResultEvent: events.ToolResultEvent{ParentID: ev.ID, Name: ev.Name},
			Error:           "поиск на сегодня исчерпан — инструменты на этот ход закрыты, ответь текстом без поиска",
		}, nil
	}
	result, err := next(ev, cx)
	// m16: неуспешный поиск не тратит ни суточный счётчик, ни бюджет хода —
	// резерв возвращается. В счётчик идёт только успешный вызов.
	if err != nil || isToolFailure(result) {
		m.mu.Lock()
		m.remaining++
		m.mu.Unlock()
		return result, err
	}
	if ierr := (web.UsageStore{}).IncrementSearch(context.Background(), m.db); ierr != nil && m.logger != nil {
		m.logger.Printf("веб: поиск не учтён в суточном счётчике: %v", ierr)
	}
	return result, err
}

// isToolFailure — вызов завершился ошибкой инструмента (сеть/провайдер).
func isToolFailure(result events.Event) bool {
	_, failed := events.AsValue(result).(events.ToolErrorEvent)
	return failed
}

// imageVerdict — решение проверки картинки хода ДО применения (41.2 M1):
// проверенный адрес и подпись со страницы. Пустой URL — картинки нет (не было
// или отклонена). Пишется в model_output, поэтому восстановление качает только
// проверенный адрес и не теряет alt (m18).
type imageVerdict struct {
	URL string
	Alt string
}

// verifyImage проверяет адрес картинки: полный http(s) из выдачи этого хода и
// наличие fetch-слоя. Любая проблема мягко обнуляет картинку (ход живёт, в лог
// — причина). allowed nil — проверка пропускается только для внутренних
// тестовых путей; восстановление берёт сохранённый вердикт, а не nil.
func (r *Runner) verifyImage(turnID int64, rawURL, alt string, allowed map[string]bool) imageVerdict {
	u := normalizeCandidateURL(strings.TrimSpace(rawURL))
	if u == "" {
		return imageVerdict{}
	}
	if allowed != nil && !allowed[u] {
		r.logf("ход %d: image_url %q не встречался в выдаче веб-инструментов этого хода — картинка не отправлена (36.2)",
			turnID, redactURLSecrets(u))
		return imageVerdict{}
	}
	if r.ImageFetcher == nil {
		r.logf("ход %d: image_url %q есть, но fetch-слой картинок не настроен — картинка не отправлена",
			turnID, redactURLSecrets(u))
		return imageVerdict{}
	}
	return imageVerdict{URL: u, Alt: sanitizeInlineNote(alt)}
}

// fetchImage скачивает проверенный адрес картинки через fetch-слой §15. Байты
// нужны для outbox_images; сбой скачивания ход не роняет.
func (r *Runner) fetchImage(ctx context.Context, turnID int64, v imageVerdict) *OutboxImage {
	if v.URL == "" {
		return nil
	}
	if r.ImageFetcher == nil {
		// Вердикт мог быть записан при включённом вебе, а после рестарта веб
		// выключен (m1): не паникуем — картинки просто нет.
		r.logf("ход %d: вердикт картинки есть, но fetch-слой не настроен — картинка не отправлена", turnID)
		return nil
	}
	img, err := r.ImageFetcher.FetchImage(ctx, v.URL)
	if err != nil {
		r.logf("ход %d: картинка %q не скачана — ход без картинки: %v", turnID, redactURLSecrets(v.URL), err)
		return nil
	}
	if img == nil || len(img.Bytes) == 0 {
		r.logf("ход %d: картинка %q пустая — ход без картинки", turnID, redactURLSecrets(v.URL))
		return nil
	}
	r.logf("ход %d: картинка к отправке: %s (%s, %d байт)",
		turnID, redactURLSecrets(img.URL), img.ContentType, len(img.Bytes))
	return &OutboxImage{SourceURL: img.URL, ContentType: img.ContentType, Bytes: img.Bytes, Alt: v.Alt}
}
