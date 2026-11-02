// Юнит-тесты сборки текстов пачки (без БД): пустые тексты помечаются
// плейсхолдером, фото без подписи — как фото (находка приёмки Р: модель
// получала «сообщение без текста» рядом с приложенной картинкой).
package character

import (
	"encoding/json"
	"strings"
	"testing"
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

// TestConstraintsMultiActorByDistinctAuthors — 17.9: пометка «в пачке
// несколько участников» ставится по числу РАЗЛИЧНЫХ авторов, а не по размеру
// пачки: после поднятия окон лички 2–4 реплики одного человека — норма, и
// ложное «несколько участников» сбивало модель на каждом таком ходе.
func TestConstraintsMultiActorByDistinctAuthors(t *testing.T) {
	one := []BatchEvent{
		{ActorID: 7, Text: "раз"},
		{ActorID: 7, Text: "два"},
		{ActorID: 7, Text: "три"},
	}
	if got := constraintsSection(one); strings.Contains(got, "несколько участников") {
		t.Fatalf("пачка одного автора не «несколько участников»: %s", got)
	}
	two := []BatchEvent{
		{ActorID: 7, Text: "раз"},
		{ActorID: 888, Text: "два"},
	}
	if got := constraintsSection(two); !strings.Contains(got, "несколько участников") {
		t.Fatalf("пачка двух авторов без пометки: %s", got)
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
	if got := constraintsSection(one); strings.Contains(got, "несколько участников") {
		t.Fatalf("ложная пометка нескольких участников: %s", got)
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
