// Юнит-тесты скуки в фоне (этап 23.2–23.4): выбор темы runtime'ом
// детерминирован при данном генераторе, причина запуска читается из reason,
// addendum собственного интереса разрешает share только по зацепке и честно
// говорит про выключенный веб.
package background

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/luckystrker/dio-agent/internal/character"
)

func TestPickCuriosityThemeDeterministic(t *testing.T) {
	// Темы-сиды проходят фильтр 38.8 (≥2 слов или ≥8 букв, не приветствия).
	persona := []string{"старое железо", "морские узлы"}
	memory := []string{"пластинки", "портовые краны"}
	seeds := []string{"портовые краны", "мемы про котов", "готовая еда"}

	first := PickCuriosityTheme(rand.New(rand.NewSource(7)), persona, memory, seeds)
	for i := 0; i < 20; i++ {
		if got := PickCuriosityTheme(rand.New(rand.NewSource(7)), persona, memory, seeds); got != first {
			t.Fatalf("выбор темы не детерминирован: %q и %q", first, got)
		}
	}
	if first == "" {
		t.Fatal("темы есть — выбор не должен быть пустым")
	}

	// Пустые источники пропускаются, пустые все — пусто.
	if got := PickCuriosityTheme(rand.New(rand.NewSource(1)), nil, []string{}, seeds); got == "" {
		t.Fatal("непустой источник должен дать тему")
	}
	if got := PickCuriosityTheme(rand.New(rand.NewSource(1)), nil, []string{}); got != "" {
		t.Fatalf("источников нет — тема пустая, получено %q", got)
	}
}

func TestCuriosityFromPersonaIncludesBio(t *testing.T) {
	bio := "Вырос во Владивостоке. Любит писать код руками, ненавидит нейросети"
	got := curiosityFromPersona(character.Persona{
		Interests: []string{"море"},
		SeedBio:   &bio,
	})
	want := map[string]bool{"море": true, "Вырос во Владивостоке": true,
		"Любит писать код руками": true, "ненавидит нейросети": true}
	if len(got) != len(want) {
		t.Fatalf("темы из персоны = %v, хочу %v", got, want)
	}
	for _, theme := range got {
		if !want[theme] {
			t.Fatalf("лишняя тема %q в %v", theme, got)
		}
	}
}

func TestIntentionOrigin(t *testing.T) {
	if got := intentionOrigin(Intention{Kind: KindExplore, Reason: boredomReasonPrefix + "давно тихо"}); got != "boredom" {
		t.Fatalf("explore от скуки: origin=%q", got)
	}
	if got := intentionOrigin(Intention{Kind: KindExplore, Reason: "тема из разговора"}); got != "conversation" {
		t.Fatalf("explore из разговора: origin=%q", got)
	}
	// reason у разговорного explore пишет модель: подделка префикса не должна
	// выдавать его за поиск интереса (addendum мягче разрешает answer/share).
	turnID := int64(7)
	if got := intentionOrigin(Intention{Kind: KindExplore, Reason: boredomReasonPrefix + "подделка", RelatedTurnID: &turnID}); got != "conversation" {
		t.Fatalf("модель подделала метку скуки: origin=%q", got)
	}
	if got := intentionOrigin(Intention{Kind: KindShare, Reason: "x"}); got != "" {
		t.Fatalf("у share нет причины-источника explore: origin=%q", got)
	}
}

func TestExploreAddendumForBoredom(t *testing.T) {
	s := &Scheduler{}
	text := s.exploreAddendumFor(Intention{Kind: KindExplore, Reason: boredomReasonPrefix + "давно тихо"})
	for _, want := range []string{"прикинь", "decision=answer", "Веб сейчас недоступен", "источник назначит runtime"} {
		if !strings.Contains(text, want) {
			t.Fatalf("addendum скуки не содержит %q: %s", want, text)
		}
	}
	// Обычный explore по-прежнему ничего не отправляет.
	usual := s.exploreAddendumFor(Intention{Kind: KindExplore, Reason: "тема из разговора"})
	if !strings.Contains(usual, "НИЧЕГО не отправляй") {
		t.Fatalf("addendum разговорного explore потерял запрет на отправку: %s", usual)
	}
}
