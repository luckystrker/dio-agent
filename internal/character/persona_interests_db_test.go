//go:build db

// БД-тесты приёмки этапа 13.7 (секция «Интересы»): сравнение версий персоны —
// по содержимому интересов, а не по длине; дефолтная персона на пустой базе
// доезжает из БД с непустыми интересами. Запуск:
//
//	go test -tags db -p 1 ./internal/character/ -run TestPersona
//
// против Postgres из docker-compose (TEST_DATABASE_URL, на машине разработчика 5433).
package character

import (
	"context"
	"slices"
	"testing"
)

// Одна длина, другое содержимое — обязана создаться новая версия с новым
// содержимым. Мутация «сравнение интересов только по длине» не должна
// переживать набор: перестановка и замена одного элемента меняют смысл.
func TestPersonaInterestsSameLengthDifferentContent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewPersonaRepo()

	base := PersonaSource{
		Name: "Dio", Description: "d", SpeechStyle: "s", Values: []string{"v"},
		MoodBaseline: Mood{},
		Interests:    []string{"старое железо", "море"},
	}
	if _, changed, err := repo.EnsureFromSource(ctx, pool, base); err != nil || !changed {
		t.Fatalf("первая версия не создана: changed=%v err=%v", changed, err)
	}

	// Перестановка той же длины: порядок — тоже содержимое.
	reordered := base
	reordered.Interests = []string{"море", "старое железо"}
	p, changed, err := repo.EnsureFromSource(ctx, pool, reordered)
	if err != nil || !changed {
		t.Fatalf("перестановка интересов той же длины не создала версию: changed=%v err=%v", changed, err)
	}
	if !slices.Equal(p.Interests, []string{"море", "старое железо"}) {
		t.Fatalf("новая версия с чужим содержимым: %v", p.Interests)
	}

	// Замена одного элемента при той же длине.
	replaced := base
	replaced.Interests = []string{"старое железо", "ржавые замки"}
	p, changed, err = repo.EnsureFromSource(ctx, pool, replaced)
	if err != nil || !changed {
		t.Fatalf("замена одного интереса не создала версию: changed=%v err=%v", changed, err)
	}
	if !slices.Equal(p.Interests, []string{"старое железо", "ржавые замки"}) {
		t.Fatalf("новая версия не с тем содержимым, что подавали: %v", p.Interests)
	}

	// Активная запись — с последним содержимым, не с первым.
	active, err := repo.Active(ctx, pool)
	if err != nil {
		t.Fatalf("активная персона не прочитана: %v", err)
	}
	if !slices.Equal(active.Interests, []string{"старое железо", "ржавые замки"}) {
		t.Fatalf("активная персона не с последним содержимым: %v", active.Interests)
	}
}

// Дефолт на пустой базе: интересы дефолта доезжают из БД целиком (не пустой
// список, не искажённый список).
func TestPersonaDefaultInterestsOnEmptyBase(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	p, err := (NewPersonaRepo()).Active(ctx, pool)
	if err != nil {
		t.Fatalf("активная персона на пустой базе не создана: %v", err)
	}
	if len(p.Interests) == 0 {
		t.Fatal("дефолтная персона без интересов — интересы не доехали из БД")
	}
	if !slices.Equal(p.Interests, DefaultPersona.Interests) {
		t.Fatalf("интересы дефолта исказились: %v, ожидались %v", p.Interests, DefaultPersona.Interests)
	}
}
