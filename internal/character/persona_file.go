// Файл личности (config/persona.md) — источник истины (§7.1): правка файла
// и перезапуск процесса создают новую версию персоны в БД (persona.go).
// Парсер строгий: любая ошибка в файле валит запуск с сообщением, называющим
// секцию/поле. Поведение перенесено из TS-справочника src/character/persona-file.ts
// (этап 7.1): секции «## …», первый заголовок выигрывает, HTML-комментарии
// вырезаются, ключи секций не наследуют чужих свойств (карта, не глобальные имена).
package character

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Обязательные и известные секции persona-файла (§7.1).
var (
	personaRequiredSections = []string{"Имя", "Описание", "Манера речи", "Ценности", "Настроение (базовое)"}
	personaKnownSections    = append(append([]string{}, personaRequiredSections...), "Биография", "Интересы", "Стикеры")
)

// commentPattern — HTML-комментарии (подсказки редактору) в файл не попадают.
var commentPattern = regexp.MustCompile(`(?s)<!--.*?-->`)

// sectionHeaderPattern — строка заголовка секции «## …».
var sectionHeaderPattern = regexp.MustCompile(`(?m)^## `)

// dashPrefix — маркер пункта списка «- » в секции «Ценности».
var dashPrefix = regexp.MustCompile(`^-\s*`)

// PersonaSource — персона без служебных полей: то, что задаёт файл.
type PersonaSource struct {
	Name         string
	Description  string
	SpeechStyle  string
	Values       []string
	MoodBaseline Mood
	// SeedBio — принятая биография (persona_seed; подаётся как прожитая —
	// философия №4, решение 2026-10-31); nil — секции нет.
	SeedBio *string
	// Interests — темы, о которых персонаж говорит охотно (§7.1, решение
	// 2026-10-31: биография — фон, интересы — другой содержательный кусок);
	// nil — секции нет.
	Interests []string
	// Stickers — стартовый набор коллекции стикеров (этап 25.3): строки
	// «file_id | эмодзи | заметка». Пустая секция допустима — владелец
	// заполнит file_id своими стикерами.
	Stickers []SourceSticker
}

// splitSections разбирает текст на секции по заголовкам «## …». Первое
// вхождение заголовка выигрывает, повтор — игнорируется (поведение Map в TS).
func splitSections(text string) map[string]string {
	withoutComments := commentPattern.ReplaceAllString(text, "")
	sections := map[string]string{}
	parts := sectionHeaderPattern.Split(withoutComments, -1)
	if len(parts) <= 1 {
		return sections
	}
	for _, part := range parts[1:] {
		header, body := part, ""
		if nl := strings.IndexByte(part, '\n'); nl >= 0 {
			header, body = part[:nl], part[nl+1:]
		}
		header = strings.TrimSpace(header)
		if _, seen := sections[header]; !seen {
			sections[header] = strings.TrimSpace(body)
		}
	}
	return sections
}

// parseMoodLine читает «поле: число» в секции настроения; запятая допускается
// как десятичный разделитель (поведение TS). Любое отклонение — ошибка с именем поля.
func parseMoodLine(body, field string, min, max float64) (float64, error) {
	lineRe := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(field) + `\s*:\s*(.+)$`)
	match := lineRe.FindStringSubmatch(body)
	if match == nil {
		return 0, fmt.Errorf("секция «Настроение (базовое)»: нет строки %s:", field)
	}
	raw := strings.TrimSpace(match[1])
	value, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", "."), 64)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf(
			"секция «Настроение (базовое)»: %s должно быть числом от %s до %s, получено: %s",
			field, formatBound(min), formatBound(max), raw)
	}
	return value, nil
}

// formatBound печатает границу диапазона как в сообщении об ошибке TS (без лишних нулей).
func formatBound(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ParsePersonaFile разбирает текст config/persona.md в источник персоны.
// Любая ошибка — явная, с именем секции/поля: невалидный файл останавливает
// запуск (§7.1), а не тихо подменяет личность умолчанием.
func ParsePersonaFile(text string) (PersonaSource, error) {
	sections := splitSections(text)

	known := map[string]bool{}
	for _, h := range personaKnownSections {
		known[h] = true
	}
	var unknown []string
	for header := range sections {
		if !known[header] {
			unknown = append(unknown, header)
		}
	}
	if len(unknown) > 0 {
		return PersonaSource{}, fmt.Errorf(
			"persona-файл: неизвестная секция «%s» — допустимы: %s",
			unknown[0], strings.Join(personaKnownSections, ", "))
	}

	var missing []string
	for _, h := range personaRequiredSections {
		if _, ok := sections[h]; !ok {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		return PersonaSource{}, fmt.Errorf("persona-файл: нет обязательной секции «%s»", missing[0])
	}

	name := sections["Имя"]
	description := sections["Описание"]
	speechStyle := sections["Манера речи"]
	if name == "" || description == "" || speechStyle == "" {
		return PersonaSource{}, fmt.Errorf("persona-файл: секции «Имя», «Описание» и «Манера речи» не должны быть пустыми")
	}

	var values []string
	for _, line := range strings.Split(sections["Ценности"], "\n") {
		line = strings.TrimSpace(dashPrefix.ReplaceAllString(strings.TrimSpace(line), ""))
		if line != "" {
			values = append(values, line)
		}
	}
	if len(values) == 0 {
		return PersonaSource{}, fmt.Errorf("persona-файл: секция «Ценности» пуста — нужен хотя бы один пункт списком (- пункт)")
	}

	moodBody := sections["Настроение (базовое)"]
	valence, err := parseMoodLine(moodBody, "valence", -1, 1)
	if err != nil {
		return PersonaSource{}, err
	}
	arousal, err := parseMoodLine(moodBody, "arousal", 0, 1)
	if err != nil {
		return PersonaSource{}, err
	}
	socialEnergy, err := parseMoodLine(moodBody, "socialEnergy", 0, 1)
	if err != nil {
		return PersonaSource{}, err
	}

	// биография необязательна; пустая секция = нет биографии
	var seedBio *string
	if raw := sections["Биография"]; strings.TrimSpace(raw) != "" {
		bio := raw
		seedBio = &bio
	}

	// интересы необязательны; секция есть — заполняй списком, как «Ценности»
	var interests []string
	if raw, ok := sections["Интересы"]; ok {
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(dashPrefix.ReplaceAllString(strings.TrimSpace(line), ""))
			if line != "" {
				interests = append(interests, line)
			}
		}
		if len(interests) == 0 {
			return PersonaSource{}, fmt.Errorf(
				"persona-файл: секция «Интересы» пуста — нужен хотя бы один пункт списком (- пункт) или удалите секцию")
		}
	}

	// стикеры необязательны: секция есть — строки «file_id | эмодзи | заметка»;
	// пустая секция — коллекция без стартового набора (этап 25.3).
	stickers, err := parseStickerSection(sections["Стикеры"])
	if err != nil {
		return PersonaSource{}, err
	}

	return PersonaSource{
		Name:         name,
		Description:  description,
		SpeechStyle:  speechStyle,
		Values:       values,
		MoodBaseline: Mood{Valence: valence, Arousal: arousal, SocialEnergy: socialEnergy},
		SeedBio:      seedBio,
		Interests:    interests,
		Stickers:     stickers,
	}, nil
}

// parseStickerSection разбирает секцию «Стикеры»: по строке на стикер,
// «file_id | эмодзи | заметка». file_id обязателен (по нему Telegram отдаёт
// файл), эмодзи и заметка — подсказки модели и владельцу.
func parseStickerSection(raw string) ([]SourceSticker, error) {
	var out []SourceSticker
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(dashPrefix.ReplaceAllString(strings.TrimSpace(line), ""))
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			return nil, fmt.Errorf(
				"persona-файл: секция «Стикеры»: строка %q не в формате «file_id | эмодзи | заметка»", line)
		}
		fileID := strings.TrimSpace(parts[0])
		if fileID == "" {
			return nil, fmt.Errorf("persona-файл: секция «Стикеры»: пустой file_id в строке %q", line)
		}
		out = append(out, SourceSticker{
			FileID: fileID,
			Emoji:  strings.TrimSpace(parts[1]),
			Note:   strings.TrimSpace(parts[2]),
		})
	}
	return out, nil
}
