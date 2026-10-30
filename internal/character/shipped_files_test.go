// Юнит-тест shipped-файлов §7.1: config/persona.md обязан парситься строгим
// парсером, config/system-prompt.md — рендериться без ошибок при живом
// состоянии; правка файлов не должна уметь останавливать запуск незаметно.
package character

import (
	"os"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("файл %s не читается: %v", path, err)
	}
	return string(raw)
}

func TestShippedPersonaFileParses(t *testing.T) {
	src, err := ParsePersonaFile(readRepoFile(t, "../../config/persona.md"))
	if err != nil {
		t.Fatalf("config/persona.md не парсится: %v", err)
	}
	if src.Name == "" || src.Description == "" || src.SpeechStyle == "" || len(src.Values) == 0 {
		t.Fatalf("config/persona.md неполон: %+v", src)
	}
}

func TestShippedSystemPromptRenders(t *testing.T) {
	template := readRepoFile(t, "../../config/system-prompt.md")
	persona := Persona{
		Name:        "Dio",
		Description: "Живой собеседник.",
		SpeechStyle: "Кратко.",
		Values:      []string{"честность"},
	}
	// Живое состояние: mood и отношение есть.
	out, err := BuildSystemPrompt(persona, PromptExtras{
		MoodLine:         "в ровном настроении",
		RelationshipLine: "почти незнаком",
	}, template)
	if err != nil {
		t.Fatalf("config/system-prompt.md не рендерится: %v", err)
	}
	for _, want := range []string{"Ты — Dio", "в ровном настроении", "почти незнаком", "Правила поведения"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в промпте из shipped-шаблона нет %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "{{") || strings.Contains(out, "<!--") {
		t.Fatalf("в промпте остались метки или комментарии:\n%s", out)
	}
	// Обязательные установки §7.1 входят через шаблон (решение этапа 7).
	if !strings.Contains(out, "не проговаривай механически") {
		t.Fatalf("обязательные установки §7.1 не в шаблоне:\n%s", out)
	}
}
