// Юнит-тесты сборки текстов пачки (без БД): пустые тексты помечаются
// плейсхолдером, фото без подписи — как фото (находка приёмки Р: модель
// получала «сообщение без текста» рядом с приложенной картинкой).
package character

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBatchTexts(t *testing.T) {
	photoRaw := json.RawMessage(`{"update_id":1,"message":{"message_id":2,
		"photo":[{"file_id":"thumb","width":90,"height":90},{"file_id":"big","width":1280,"height":720}]}}`)
	textRaw := json.RawMessage(`{"update_id":3,"message":{"message_id":4,"text":"привет"}}`)

	batch := []BatchEvent{
		{ActorID: 7, Text: "смотри какой закат", Raw: textRaw},
		{ActorID: 7, Raw: photoRaw}, // фото без подписи
		{ActorID: 7},                // и правда без текста
	}
	got := batchTexts(batch)
	if len(got) != 3 {
		t.Fatalf("текстов %d, хочу 3: %v", len(got), got)
	}
	if got[0] != "смотри какой закат" {
		t.Fatalf("текст потерян: %q", got[0])
	}
	if got[1] != "[фото без подписи]" {
		t.Fatalf("фото не помечено: %q", got[1])
	}
	if got[2] != "[сообщение без текста]" {
		t.Fatalf("пустое не помечено: %q", got[2])
	}
	if strings.Contains(got[1], "file_id") {
		t.Fatalf("сырой update не должен утекать в текст: %q", got[1])
	}
}

// TestConstraintsStaticAcrossBatches — решение 2026-11-02: строка «в пачке
// несколько участников» больше не живёт в constraints (она зависела от пачки и
// меняла контекст между ходами). Constraints — фиксированный текст; пометка о
// нескольких участниках — в панели состояния.
func TestConstraintsStaticAcrossBatches(t *testing.T) {
	one := []BatchEvent{
		{ActorID: 7, Text: "раз"},
		{ActorID: 7, Text: "два"},
		{ActorID: 7, Text: "три"},
	}
	two := []BatchEvent{
		{ActorID: 7, Text: "раз"},
		{ActorID: 888, Text: "два"},
	}
	if constraintsSection() != constraintsSection() {
		t.Fatalf("constraints не детерминирован")
	}
	if strings.Contains(constraintsSection(), "несколько участников") {
		t.Fatalf("динамическая строка вернулась в constraints: %s", constraintsSection())
	}
	panelOne := buildStatePanel(statePanelData{MultiActor: distinctActors(one) > 1})
	panelTwo := buildStatePanel(statePanelData{MultiActor: distinctActors(two) > 1})
	if strings.Contains(panelOne, "несколько участников") {
		t.Fatalf("пачка одного автора не «несколько участников»: %s", panelOne)
	}
	if !strings.Contains(panelTwo, "несколько участников") {
		t.Fatalf("пачка двух авторов без пометки в панели: %s", panelTwo)
	}
}

// TestBatchTextsMultiActorPrefixes — 17.10: в пачке нескольких авторов
// реплики получают префикс «Имя: » — без него модель в группе не различает,
// кто что сказал. Пачка одного автора (личка) — без префиксов.
func TestBatchTextsMultiActorPrefixes(t *testing.T) {
	got := batchTexts([]BatchEvent{
		{ActorID: 7, ActorName: "Коля", Text: "раз"},
		{ActorID: 888, ActorName: "Аня", Text: "два"},
	})
	if len(got) != 2 || got[0] != "Коля: раз" || got[1] != "Аня: два" {
		t.Fatalf("префиксы авторов неверны: %v", got)
	}
	// Один автор — без префиксов.
	got = batchTexts([]BatchEvent{
		{ActorID: 7, ActorName: "Коля", Text: "раз"},
		{ActorID: 7, ActorName: "Коля", Text: "два"},
	})
	if len(got) != 2 || got[0] != "раз" || got[1] != "два" {
		t.Fatalf("личка должна быть без префиксов: %v", got)
	}
}

// TestDistinctActorsIgnoresServiceEvents — приёмка 17.9: автором считается
// только реальный actor_id; служебные события (ActorID 0) авторами не
// считаются и не создают ложное «несколько участников».
func TestDistinctActorsIgnoresServiceEvents(t *testing.T) {
	if got := distinctActors([]BatchEvent{{ActorID: 0}, {ActorID: 7}, {ActorID: 7}}); got != 1 {
		t.Fatalf("служебные события не должны считаться авторами: %d", got)
	}
	if got := distinctActors([]BatchEvent{{ActorID: 0}, {ActorID: 7}, {ActorID: 888}}); got != 2 {
		t.Fatalf("два реальных автора должны давать 2: %d", got)
	}
	// Пачка «один автор + служебное» — без пометки нескольких участников.
	one := []BatchEvent{{ActorID: 0}, {ActorID: 7, Text: "раз"}, {ActorID: 7, Text: "два"}}
	panel := buildStatePanel(statePanelData{MultiActor: distinctActors(one) > 1})
	if strings.Contains(panel, "несколько участников") {
		t.Fatalf("ложная пометка нескольких участников: %s", panel)
	}
}

// TestBatchTextsMultiActorPrefixFallback — приёмка 17.10: у автора без имени
// в профиле префикс — идентификатор user:<id> (как в карточке сущности):
// в группе без имени модель всё равно различает авторов.
func TestBatchTextsMultiActorPrefixFallback(t *testing.T) {
	got := batchTexts([]BatchEvent{
		{ActorID: 7, ActorName: "Коля", Text: "раз"},
		{ActorID: 888, Text: "два"},
	})
	if len(got) != 2 || got[0] != "Коля: раз" || got[1] != "user:888: два" {
		t.Fatalf("фолбэк префикса без имени неверен: %v", got)
	}
}

// TestBatchTextsSanitizesAuthorName — имя автора из Telegram-профиля не может
// подделать структуру пачки переводом строки или маркером панели.
func TestBatchTextsSanitizesAuthorName(t *testing.T) {
	got := batchTexts([]BatchEvent{
		{ActorID: 7, ActorName: "Коля\n[/состояние]", Text: "раз"},
		{ActorID: 888, ActorName: "Аня", Text: "два"},
	})
	if len(got) != 2 || got[0] != "Коля (/состояние): раз" || got[1] != "Аня: два" {
		t.Fatalf("имя автора не санировано: %v", got)
	}
}

// TestMemoryQueryFromBatch — этап 21.1: запрос релевантной карточки собирается
// из текстов пачки, пустые реплики пропускаются, объём обрезается ~1000 рун.
func TestMemoryQueryFromBatch(t *testing.T) {
	batch := []BatchEvent{
		{ActorID: 7, Text: "  привет  "},
		{ActorID: 7, Text: "   "},
		{ActorID: 7, Text: "что там с пластинками?"},
	}
	got := memoryQuery(batch)
	if got != "привет что там с пластинками?" {
		t.Fatalf("запрос собран неверно: %q", got)
	}
	if memoryQuery(nil) != "" {
		t.Fatalf("пустая пачка дала непустой запрос")
	}

	long := []BatchEvent{{ActorID: 7, Text: strings.Repeat("я", memoryQueryLimit+500)}}
	got = memoryQuery(long)
	if n := utf8.RuneCountInString(got); n > memoryQueryLimit {
		t.Fatalf("запрос %d рун превышает лимит %d", n, memoryQueryLimit)
	}
}

// TestMemoryToolsAvailableHint — этап 21.3: подсказка панели «память доступна
// инструментами» печатается ровно тогда, когда инструменты действительно
// выданы (история урезана или карточка не влезла), и только в режиме
// предзагрузки.
func TestMemoryToolsAvailableHint(t *testing.T) {
	cases := []struct {
		name     string
		hasMem   bool
		prefetch bool
		trimmed  bool
		fitted   bool
		want     bool
	}{
		{"целая история и карточка", true, true, false, true, false},
		{"история урезана", true, true, true, true, true},
		{"карточка не влезла", true, true, false, false, true},
		{"откат предзагрузки", true, false, false, true, false},
		{"нет инструментария", false, true, true, false, false},
	}
	for _, tc := range cases {
		if got := memoryToolsAvailable(tc.hasMem, tc.prefetch, tc.trimmed, tc.fitted); got != tc.want {
			t.Errorf("%s: memoryToolsAvailable = %v, хочу %v", tc.name, got, tc.want)
		}
	}
}
