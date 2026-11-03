// Юнит-тесты секции «Темы для любопытства» persona-файла (этап 23.2):
// необязательная секция, по теме на строку, пустая секция — ошибка запуска.
package character

import (
	"strings"
	"testing"
)

func TestParsePersonaFileCuriosity(t *testing.T) {
	text := validPersonaText() + "\n## Темы для любопытства\n- портовые краны\nстарые пластинки\n"
	src, err := ParsePersonaFile(text)
	if err != nil {
		t.Fatalf("секция тем не разобрана: %v", err)
	}
	want := []string{"портовые краны", "старые пластинки"}
	if len(src.Curiosity) != len(want) {
		t.Fatalf("темы = %v, хочу %v", src.Curiosity, want)
	}
	for i := range want {
		if src.Curiosity[i] != want[i] {
			t.Fatalf("тема %d = %q, хочу %q", i, src.Curiosity[i], want[i])
		}
	}
}

func TestParsePersonaFileCuriosityOptional(t *testing.T) {
	src, err := ParsePersonaFile(validPersonaText())
	if err != nil {
		t.Fatalf("без секции тем файл не разобран: %v", err)
	}
	if src.Curiosity != nil {
		t.Fatalf("секции нет — тем быть не должно: %v", src.Curiosity)
	}
}

func TestParsePersonaFileCuriosityEmpty(t *testing.T) {
	text := validPersonaText() + "\n## Темы для любопытства\n\n"
	_, err := ParsePersonaFile(text)
	if err == nil || !strings.Contains(err.Error(), "Темы для любопытства» пуста") {
		t.Fatalf("хочу ошибку про пустую секцию тем, получено: %v", err)
	}
}

// Темы любопытства — настройка процесса, как расписание: в версию персоны в
// БД не входят, поэтому отличие только в них новую версию не создаёт.
func TestPersonaMatchesSourceIgnoresCuriosity(t *testing.T) {
	p := Persona{Name: "Dio", Description: "d", SpeechStyle: "s", Values: []string{"v"}}
	src := PersonaSource{Name: "Dio", Description: "d", SpeechStyle: "s", Values: []string{"v"},
		Curiosity: []string{"краны", "винил"}}
	if !personaMatchesSource(p, src) {
		t.Fatal("темы любопытства не должны влиять на версию персоны")
	}
}
