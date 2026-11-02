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
	// Живое состояние передаётся рендеру, но shipped-шаблон обязан его
	// игнорировать: system статичен байт-в-байт (решение 2026-11-02).
	live := PromptExtras{
		MoodLine:         "в ровном настроении",
		RelationshipLine: "почти незнаком",
		ActorLine:        "Данил",
		MemoryBlock:      "любит пластинки",
	}
	out, err := BuildSystemPrompt(persona, live, template)
	if err != nil {
		t.Fatalf("config/system-prompt.md не рендерится: %v", err)
	}
	for _, want := range []string{"Ты — Dio", "Правила поведения", "Метки времени в истории"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в промпте из shipped-шаблона нет %q:\n%s", want, out)
		}
	}
	for _, dynamic := range []string{"в ровном настроении", "почти незнаком", "Данил", "пластинки", "{{", "<!--"} {
		if strings.Contains(out, dynamic) {
			t.Fatalf("динамика/метка %q попала в system:\n%s", dynamic, out)
		}
	}
	if strings.HasSuffix(strings.TrimSpace(out), "Отношение к собеседнику") {
		t.Fatalf("system заканчивается динамическим блоком:\n%s", out)
	}
	// Обязательные установки §7.1 входят через шаблон (решение этапа 7).
	if !strings.Contains(out, "не проговаривай механически") {
		t.Fatalf("обязательные установки §7.1 не в шаблоне:\n%s", out)
	}
	// Same template, другое состояние — тот же байт-в-байт system.
	other, err := BuildSystemPrompt(persona, PromptExtras{
		MoodLine:         "зол и устал",
		RelationshipLine: "враг",
		ActorLine:        "Аня",
	}, template)
	if err != nil {
		t.Fatalf("config/system-prompt.md не рендерится: %v", err)
	}
	if out != other {
		t.Fatalf("shipped system меняется вместе с состоянием хода")
	}
}
