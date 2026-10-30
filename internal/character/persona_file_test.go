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

## Биография (вымысел)
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
	text := strings.Replace(validPersonaText(), "\n## Биография (вымысел)\nВырос в портовом городе.\n", "", 1)
	src, err := ParsePersonaFile(text)
	if err != nil {
		t.Fatalf("файл без биографии отвергнут: %v", err)
	}
	if src.SeedBio != nil {
		t.Fatalf("биографии не должно быть, получено: %v", *src.SeedBio)
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
