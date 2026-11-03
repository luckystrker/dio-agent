// Юнит-тесты событий жизни (этап 24, решение владельца 2026-11-03):
// частота генератора (8–14 ч со случайным разбросом через инжектируемый rand),
// обход тихих блоков расписания, строгий proposal-контракт модели (одно
// бытовое событие, mood_delta с основанием, флаг «хочу рассказать») и тема
// share-намерения. Сеть и БД здесь не нужны — это гейт без Postgres.
package background

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/character"
)

// TestPickLifeEventDelayRange — пауза между событиями попадает в 8–14 часов и
// детерминирована при одинаковом сиде (инжектируемый rand — как у скуки).
func TestPickLifeEventDelayRange(t *testing.T) {
	for seed := int64(0); seed < 50; seed++ {
		first := PickLifeEventDelay(rand.New(rand.NewSource(seed)))
		if first < LifeEventDelayMin || first > LifeEventDelayMax {
			t.Fatalf("сид %d: пауза %v вне [%v, %v]", seed, first, LifeEventDelayMin, LifeEventDelayMax)
		}
		if again := PickLifeEventDelay(rand.New(rand.NewSource(seed))); again != first {
			t.Fatalf("сид %d: пауза не детерминирована: %v и %v", seed, first, again)
		}
	}
	// Разные сиды должны давать разные паузы, а не константу.
	var sawDifferent bool
	base := PickLifeEventDelay(rand.New(rand.NewSource(1)))
	for seed := int64(2); seed < 20; seed++ {
		if PickLifeEventDelay(rand.New(rand.NewSource(seed))) != base {
			sawDifferent = true
			break
		}
	}
	if !sawDifferent {
		t.Fatal("разброс паузы отсутствует: все сиды дают одно значение")
	}
}

// TestShiftOutOfQuiet — событие, попавшее в тихий блок, переносится на его
// конец: ночью персонаж спит, а не проживает события.
func TestShiftOutOfQuiet(t *testing.T) {
	loc := moscowLoc(t)
	sched := quietSchedule()
	persona := character.Persona{Schedule: &sched}

	at := time.Date(2026, 11, 3, 22, 0, 0, 0, loc)
	if got := shiftOutOfQuiet(at, persona, loc); !got.Equal(at) {
		t.Fatalf("бодрый час сдвинут: %v, хочу %v", got, at)
	}

	night := at.Add(5 * time.Hour) // 03:00 следующих суток — тихий блок
	want := time.Date(2026, 11, 4, 8, 0, 0, 0, loc)
	if got := shiftOutOfQuiet(night, persona, loc); !got.Equal(want) {
		t.Fatalf("ночное событие не перенесено на конец тишины: %v, хочу %v", got, want)
	}

	// Расписания нет — сдвигать некуда, время остаётся.
	if got := shiftOutOfQuiet(night, character.Persona{}, loc); !got.Equal(night) {
		t.Fatalf("без расписания время изменилось: %v", got)
	}
}

// TestParseLifeEventProposal — строгий контракт: только текст, mood_delta с
// основанием и необязательный флаг «хочу рассказать»; мусор и лишние поля
// отклоняются.
func TestParseLifeEventProposal(t *testing.T) {
	good := `{"text":"сегодня на работе сгорел блок питания, пришлось чинить на коленке",` +
		`"mood_delta":{"valence":-0.1,"arousal":0.2,"reason":"сначала психанул, потом азарт"},` +
		`"want_to_tell":true}`
	p, err := parseLifeEventProposal([]byte(good))
	if err != nil {
		t.Fatalf("валидное событие отклонено: %v", err)
	}
	if !p.WantToTell || p.MoodDelta == nil || p.MoodDelta.Reason != "сначала психанул, потом азарт" {
		t.Fatalf("событие разобрано неверно: %+v", p)
	}
	if p.MoodDelta.Valence == nil || *p.MoodDelta.Valence != -0.1 {
		t.Fatalf("valence потерян: %+v", p.MoodDelta)
	}

	bad := map[string]string{
		"пустой текст":     `{"text":"  ","mood_delta":{"reason":"x"}}`,
		"текст длиннее":    `{"text":"` + strings.Repeat("я", lifeEventTextLimit+1) + `","mood_delta":{"reason":"x"}}`,
		"без mood_delta":   `{"text":"сосед опять сверлит"}`,
		"пустое основание": `{"text":"сосед опять сверлит","mood_delta":{"valence":0.1,"reason":" "}}`,
		"лишнее поле":      `{"text":"сосед опять сверлит","mood_delta":{"reason":"x"},"urgency":9}`,
		"текст не строка":  `{"text":42,"mood_delta":{"reason":"x"}}`,
		"дельта не объект": `{"text":"сосед опять сверлит","mood_delta":"x"}`,
		"хвост после JSON": `{"text":"сосед опять сверлит","mood_delta":{"reason":"x"}} мусор`,
	}
	for name, body := range bad {
		if _, err := parseLifeEventProposal([]byte(body)); err == nil {
			t.Fatalf("%s: proposal принят, хочу ошибку", name)
		}
	}
}

// TestLifeEventShareTopic — тема share-намерения короткая и берётся из самого
// события: одна фраза, без переносов, не длиннее потолка темы.
func TestLifeEventShareTopic(t *testing.T) {
	topic := lifeEventShareTopic("Сегодня пришла зарплата. Наконец-то закрыл долг за интернет, ещё и на газировку осталось.")
	if topic == "" {
		t.Fatal("тема share пуста")
	}
	if strings.ContainsAny(topic, "\n\r") {
		t.Fatalf("тема share многострочная: %q", topic)
	}
	if runes := []rune(topic); len(runes) > lifeEventShareTopicLimit {
		t.Fatalf("тема share длиннее %d рун: %q", lifeEventShareTopicLimit, topic)
	}
	if !strings.Contains(strings.ToLower(topic), "зарплата") {
		t.Fatalf("тема share не отражает событие: %q", topic)
	}
}
