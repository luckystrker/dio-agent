// Тесты веба в разговорном ходе (этап 36.1–36.2): выдача инструментов по
// решению харнесса и суточному лимиту, подсказки панели, сбор URL из выдачи
// этого хода и отказ картинке, которой в выдаче не было.
package character

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/web"
)

func TestWebToolState(t *testing.T) {
	cases := []struct {
		name          string
		enabled       bool
		used, limit   int64
		readErr       error
		wantTools     bool
		wantExhausted bool
	}{
		{"веб выключен", false, 0, 15, nil, false, false},
		{"свободно", true, 3, 15, nil, true, false},
		{"ровно на лимите", true, 15, 15, nil, false, true},
		{"лимит перебран", true, 16, 15, nil, false, true},
		{"лимит не задан — дефолт", true, 14, 0, nil, true, false},
		{"счётчик не прочитан — веб не выдаём", true, 0, 15, errors.New("db down"), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools, exhausted := webToolState(tc.enabled, tc.used, tc.limit, tc.readErr)
			if tools != tc.wantTools || exhausted != tc.wantExhausted {
				t.Fatalf("webToolState = (%v,%v), хочу (%v,%v)", tools, exhausted, tc.wantTools, tc.wantExhausted)
			}
		})
	}
}

func TestPanelWebLines(t *testing.T) {
	base := statePanelData{Now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}

	panel := buildStatePanel(statePanelData{Now: base.Now, WebTools: true})
	if !strings.Contains(panel, "веб доступен инструментами") || !strings.Contains(panel, "свежий факт") {
		t.Fatalf("в панели нет подсказки о вебе: %q", panel)
	}

	panel = buildStatePanel(statePanelData{Now: base.Now, WebExhausted: true})
	if !strings.Contains(panel, "поиск на сегодня исчерпан") {
		t.Fatalf("в панели нет строки об исчерпанном поиске: %q", panel)
	}

	// Обе строки одновременно невозможны: исчерпан — инструменты не выдаются.
	panel = buildStatePanel(statePanelData{Now: base.Now, WebTools: true, WebExhausted: true})
	if strings.Contains(panel, "веб доступен инструментами") && strings.Contains(panel, "поиск на сегодня исчерпан") {
		t.Fatalf("панель противоречит сама себе: %q", panel)
	}
}

func TestCollectToolResultURLs(t *testing.T) {
	searchResult := `{"query":"погода","results":[{"title":"Погода","href":"https://example.com/weather","body":"снег"}]}`
	got := collectToolResultURLs(searchResult)
	if len(got) != 1 || got[0] != "https://example.com/weather" {
		t.Fatalf("из выдачи поиска собрано %v, хочу ссылку example.com/weather", got)
	}

	pageResult := "URL: https://example.com/a\n\nтекст\n\nКартинки на странице:\nhttps://cdn.example.com/p.jpg\nhttps://cdn.example.com/q.png."
	got = collectToolResultURLs(pageResult)
	if len(got) != 3 {
		t.Fatalf("со страницы собрано %v, хочу три адреса", got)
	}
	if got[2] != "https://cdn.example.com/q.png" {
		t.Fatalf("хвостовая точка не срезана: %q", got[2])
	}
}

// fakeImageFetcher — подмена fetch-слоя §15 в тестах хода.
type fakeImageFetcher struct {
	got []string
	img *web.Image
	err error
}

func (f *fakeImageFetcher) FetchImage(_ context.Context, rawURL string) (*web.Image, error) {
	f.got = append(f.got, rawURL)
	if f.err != nil {
		return nil, f.err
	}
	return f.img, nil
}

func newImageTestRunner(fetcher ImageFetcher) *Runner {
	return &Runner{ImageFetcher: fetcher, Logger: log.New(io.Discard, "", 0)}
}

func TestResolveImageOnlyFromThisTurn(t *testing.T) {
	fetcher := &fakeImageFetcher{img: &web.Image{URL: "https://cdn.example.com/a.jpg", ContentType: "image/jpeg", Bytes: []byte("jpeg")}}
	r := newImageTestRunner(fetcher)

	// URL не встречался в выдаче этого хода — картинка не скачивается и не уходит.
	if img := r.resolveImage(context.Background(), 1, "https://evil.example.com/x.jpg", "", map[string]bool{"https://cdn.example.com/a.jpg": true}); img != nil {
		t.Fatalf("URL не из выдачи принят: %+v", img)
	}
	if len(fetcher.got) != 0 {
		t.Fatalf("скачивание началось до проверки выдачи: %v", fetcher.got)
	}

	// Встречался — скачивается через fetch-слой.
	img := r.resolveImage(context.Background(), 1, "https://cdn.example.com/a.jpg", "", map[string]bool{"https://cdn.example.com/a.jpg": true})
	if img == nil || img.ContentType != "image/jpeg" || string(img.Bytes) != "jpeg" {
		t.Fatalf("картинка из выдачи не скачана: %+v", img)
	}
	if len(fetcher.got) != 1 || fetcher.got[0] != "https://cdn.example.com/a.jpg" {
		t.Fatalf("fetch-слой получил %v", fetcher.got)
	}
}

func TestResolveImageSoftFailures(t *testing.T) {
	// Сбой скачивания не роняет ход: картинки просто нет.
	r := newImageTestRunner(&fakeImageFetcher{err: errors.New("SSRF")})
	if img := r.resolveImage(context.Background(), 1, "https://cdn.example.com/a.jpg", "", nil); img != nil {
		t.Fatalf("сбой скачивания должен мягко обнулить картинку: %+v", img)
	}

	// Fetch-слой не настроен — ход живёт без картинки.
	r = newImageTestRunner(nil)
	if img := r.resolveImage(context.Background(), 1, "https://cdn.example.com/a.jpg", "", nil); img != nil {
		t.Fatalf("без fetch-слоя картинки быть не может: %+v", img)
	}

	// Пустой URL — ничего не делаем.
	r = newImageTestRunner(&fakeImageFetcher{})
	if img := r.resolveImage(context.Background(), 1, "  ", "", nil); img != nil {
		t.Fatalf("пустой image_url дал картинку: %+v", img)
	}
}

func TestConversationToolLimit(t *testing.T) {
	cases := []struct {
		name                    string
		memoryOn                bool
		memoryLimit, configured int
		webOn                   bool
		want                    int
	}{
		{"без веба — как раньше", true, 2, 5, false, 2},
		{"память+веб — потолок 5", true, 2, 0, true, 5},
		{"только веб — 2 поиска+2 страницы", false, 2, 0, true, 4},
		{"конфиг ниже — уважаем", true, 2, 3, true, 3},
		{"память выключена без веба", false, 2, 5, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conversationToolLimit(tc.memoryOn, tc.memoryLimit, tc.webOn, tc.configured); got != tc.want {
				t.Fatalf("лимит = %d, хочу %d", got, tc.want)
			}
		})
	}
}

func TestWebUsageMWRejectsBeyondDailyLimit(t *testing.T) {
	mw := &webUsageMW{name: "duckduckgo_search", remaining: 0}
	called := false
	next := func(*events.ToolCallEvent, *agocontext.Context) (events.Event, error) {
		called = true
		return &events.ToolResultEvent{}, nil
	}
	out, err := mw.OnToolExecution(next, &events.ToolCallEvent{ID: "c1", Name: "duckduckgo_search", Arguments: "{}"}, nil)
	if err != nil {
		t.Fatalf("отказ лимита не ошибка транспорта: %v", err)
	}
	if called {
		t.Fatal("поиск сверх суточного лимита не должен исполняться")
	}
	tero, ok := events.AsValue(out).(events.ToolErrorEvent)
	if !ok || !strings.Contains(tero.Error, "поиск на сегодня исчерпан") {
		t.Fatalf("модели нужен явный отказ лимита, получено %#v", out)
	}
}

func TestWebResultMWCollectsOnlyWebTools(t *testing.T) {
	collector := newWebURLCollector()
	mw := &webResultMW{collector: collector, searchName: "duckduckgo_search"}
	next := func(*events.ToolCallEvent, *agocontext.Context) (events.Event, error) {
		return &events.ToolResultEvent{
			Name:   "tool",
			Result: events.NewToolResult(false, "https://cdn.example.com/a.jpg"),
		}, nil
	}
	if _, err := mw.OnToolExecution(next, &events.ToolCallEvent{Name: memory.ToolSearch}, nil); err != nil {
		t.Fatalf("инструмент памяти: %v", err)
	}
	if len(collector.all()) != 0 {
		t.Fatalf("адреса инструмента памяти попали в выдачу хода: %v", collector.all())
	}
	if _, err := mw.OnToolExecution(next, &events.ToolCallEvent{Name: "duckduckgo_search"}, nil); err != nil {
		t.Fatalf("поиск: %v", err)
	}
	if !collector.all()["https://cdn.example.com/a.jpg"] {
		t.Fatalf("адрес поиска не собран: %v", collector.all())
	}
}
