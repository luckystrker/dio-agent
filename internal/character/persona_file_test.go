// Юнит-тесты парсера persona-файла: паритет поведения src/character/persona-file.ts
// (этап 7.1): строгие секции, ошибки с именами секций/полей, комментарии,
// запятая как десятичный разделитель, повтор заголовка.
package character

import (
	"strings"
	"testing"
)

func validPersonaText() string {
	return `<!-- подсказка редактору -->
# Личность

## Имя
Dio

## Описание
Живой собеседник.

## Манера речи
Кратко, живо.

## Ценности
- честность
- любопытство

## Настроение (базовое)
valence: 0,1
arousal: 0.4
socialEnergy: 0.6

## Биография
Вырос в портовом городе.
`
}

func TestParsePersonaFileValid(t *testing.T) {
	src, err := ParsePersonaFile(validPersonaText())
	if err != nil {
		t.Fatalf("ParsePersonaFile вернул ошибку: %v", err)
	}
	if src.Name != "Dio" || src.Description != "Живой собеседник." || src.SpeechStyle != "Кратко, живо." {
		t.Fatalf("простые секции прочитаны неверно: %+v", src)
	}
	if len(src.Values) != 2 || src.Values[0] != "честность" || src.Values[1] != "любопытство" {
		t.Fatalf("ценности прочитаны неверно: %v", src.Values)
	}
	if src.MoodBaseline.Valence != 0.1 {
		t.Fatalf("valence с запятой не распознан: %v", src.MoodBaseline.Valence)
	}
	if src.MoodBaseline.Arousal != 0.4 || src.MoodBaseline.SocialEnergy != 0.6 {
		t.Fatalf("mood baseline неверен: %+v", src.MoodBaseline)
	}
	if src.SeedBio == nil || strings.TrimSpace(*src.SeedBio) != "Вырос в портовом городе." {
		t.Fatalf("биография прочитана неверно: %v", src.SeedBio)
	}
}

func TestParsePersonaFileMissingSection(t *testing.T) {
	text := strings.Replace(validPersonaText(), "## Манера речи\nКратко, живо.\n\n", "", 1)
	_, err := ParsePersonaFile(text)
	if err == nil || !strings.Contains(err.Error(), "нет обязательной секции «Манера речи»") {
		t.Fatalf("хочу ошибку про отсутствующую секцию, получено: %v", err)
	}
}

func TestParsePersonaFileUnknownSection(t *testing.T) {
	text := validPersonaText() + "\n## Лишнее\nчто-то\n"
	_, err := ParsePersonaFile(text)
	if err == nil || !strings.Contains(err.Error(), "неизвестная секция «Лишнее»") {
		t.Fatalf("хочу ошибку про неизвестную секцию, получено: %v", err)
	}
}

func TestParsePersonaFileEmptyRequired(t *testing.T) {
	text := strings.Replace(validPersonaText(), "## Имя\nDio\n", "## Имя\n\n", 1)
	_, err := ParsePersonaFile(text)
	if err == nil || !strings.Contains(err.Error(), "не должны быть пустыми") {
		t.Fatalf("хочу ошибку про пустую секцию, получено: %v", err)
	}
}

func TestParsePersonaFileEmptyValues(t *testing.T) {
	text := strings.Replace(validPersonaText(), "## Ценности\n- честность\n- любопытство\n", "## Ценности\n\n", 1)
	_, err := ParsePersonaFile(text)
	if err == nil || !strings.Contains(err.Error(), "Ценности» пуста") {
		t.Fatalf("хочу ошибку про пустые ценности, получено: %v", err)
	}
}

func TestParsePersonaFileMoodErrors(t *testing.T) {
	cases := []struct {
		name string
		repl string
		want string
	}{
		{"нет строки", "valence: 0,1\n", "нет строки valence:"},
		{"вне диапазона", "valence: 0,1", "valence"},
		{"не число", "valence: abc", "valence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := validPersonaText()
			if tc.name == "нет строки" {
				text = strings.Replace(text, "valence: 0,1\n", "", 1)
			} else {
				text = strings.Replace(text, "valence: 0,1", "valence: "+map[string]string{"вне диапазона": "5", "не число": "abc"}[tc.name], 1)
			}
			_, err := ParsePersonaFile(text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("хочу ошибку %q, получено: %v", tc.want, err)
			}
		})
	}
}

func TestParsePersonaFileMoodBounds(t *testing.T) {
	// valence ∈ [-1..1], arousal/socialEnergy ∈ [0..1] — границы допустимы.
	text := validPersonaText()
	text = strings.Replace(text, "valence: 0,1", "valence: -1", 1)
	text = strings.Replace(text, "arousal: 0.4", "arousal: 0", 1)
	text = strings.Replace(text, "socialEnergy: 0.6", "socialEnergy: 1", 1)
	src, err := ParsePersonaFile(text)
	if err != nil {
		t.Fatalf("граничные значения отвергнуты: %v", err)
	}
	if src.MoodBaseline.Valence != -1 || src.MoodBaseline.Arousal != 0 || src.MoodBaseline.SocialEnergy != 1 {
		t.Fatalf("границы прочитаны неверно: %+v", src.MoodBaseline)
	}
}

func TestParsePersonaFileNoBio(t *testing.T) {
	text := strings.Replace(validPersonaText(), "\n## Биография\nВырос в портовом городе.\n", "", 1)
	src, err := ParsePersonaFile(text)
	if err != nil {
		t.Fatalf("файл без биографии отвергнут: %v", err)
	}
	if src.SeedBio != nil {
		t.Fatalf("биографии не должно быть, получено: %v", *src.SeedBio)
	}
}

func TestParsePersonaFileInterests(t *testing.T) {
	// Секция «Интересы» необязательна (§7.1, решение 2026-10-31): список
	// как в «Ценностях».
	withInterests := strings.Replace(validPersonaText(),
		"## Настроение (базовое)",
		"## Интересы\n- старое железо\n- море и порты\n\n## Настроение (базовое)", 1)
	src, err := ParsePersonaFile(withInterests)
	if err != nil {
		t.Fatalf("файл с интересами отвергнут: %v", err)
	}
	if len(src.Interests) != 2 || src.Interests[0] != "старое железо" || src.Interests[1] != "море и порты" {
		t.Fatalf("интересы прочитаны неверно: %v", src.Interests)
	}

	// Без секции — nil (как биография).
	src, err = ParsePersonaFile(validPersonaText())
	if err != nil {
		t.Fatalf("файл без интересов отвергнут: %v", err)
	}
	if src.Interests != nil {
		t.Fatalf("интересов быть не должно: %v", src.Interests)
	}

	// Пустая секция — ошибка, а не молчаливое «нет».
	empty := strings.Replace(validPersonaText(),
		"## Настроение (базовое)", "## Интересы\n\n## Настроение (базовое)", 1)
	_, err = ParsePersonaFile(empty)
	if err == nil || !strings.Contains(err.Error(), "Интересы» пуста") {
		t.Fatalf("хочу ошибку про пустые интересы, получено: %v", err)
	}
}

func TestParsePersonaFileFirstHeaderWins(t *testing.T) {
	// Повтор заголовка: первое вхождение выигрывает (паритет Map TS).
	text := strings.Replace(validPersonaText(), "## Имя\nDio\n", "## Имя\nDio\n\n## Имя\nДругое\n", 1)
	src, err := ParsePersonaFile(text)
	if err != nil {
		t.Fatalf("повтор заголовка отвергнут: %v", err)
	}
	if src.Name != "Dio" {
		t.Fatalf("второй заголовок перезаписал первый: %q", src.Name)
	}
}

func TestParsePersonaFileCommentOnlyLinesIgnored(t *testing.T) {
	text := strings.Replace(validPersonaText(), "## Описание\nЖивой собеседник.", "## Описание\n<!-- заметка -->\nЖивой собеседник.", 1)
	src, err := ParsePersonaFile(text)
	if err != nil {
		t.Fatalf("комментарий в секции отвергнут: %v", err)
	}
	if strings.Contains(src.Description, "заметка") {
		t.Fatalf("комментарий попал в описание: %q", src.Description)
	}
}

func TestParsePersonaAliases(t *testing.T) {
	text := validPersonaText() + "\n## Алиасы\nдио, дийо; dio\n"
	src, err := ParsePersonaFile(text)
	if err != nil {
		t.Fatalf("алиасы не разобраны: %v", err)
	}
	want := []string{"дио", "дийо", "dio"}
	if len(src.Aliases) != len(want) {
		t.Fatalf("алиасы: %v, хочу %v", src.Aliases, want)
	}
	for i := range want {
		if src.Aliases[i] != want[i] {
			t.Fatalf("алиасы: %v, хочу %v", src.Aliases, want)
		}
	}
	// Без секции — алиасов нет, имя из «Имя» действует всегда.
	src, err = ParsePersonaFile(validPersonaText())
	if err != nil || len(src.Aliases) != 0 {
		t.Fatalf("без секции алиасов: %v err=%v", src.Aliases, err)
	}
}
