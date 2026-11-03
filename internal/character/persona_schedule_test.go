// Юнит-тесты расписания дня (этап 22.1): необязательная секция «Расписание» в
// persona-файле, строгая валидация блоков (сутки покрыты без дырок и
// перекрытий, блок через полночь) и выбор блока по часам AGENT_TIMEZONE.
package character

import (
	"strings"
	"testing"
	"time"
)

// scheduleText собирает текст секции расписания (без остального persona-файла:
// парсер ищет только заголовок «## Расписание»).
func scheduleText(blocks ...string) string {
	return "## Расписание\n" + strings.Join(blocks, "\n") + "\n"
}

// validScheduleBlocks — расписание на сутки: ночь (тихо), утро, работа, вечер и
// тихий блок перед сном, переходящий через полночь.
func validScheduleBlocks() []string {
	return []string{
		"01:00–08:00 | arousal 0.15 | socialEnergy 0.1 | тихо | спит",
		"08:00–11:00 | arousal 0.35 | socialEnergy 0.4 | только проснулся, пьёт кофе",
		"11:00–18:00 | arousal 0.65 | socialEnergy 0.8 | на работе",
		"18:00–23:00 | arousal 0.55 | socialEnergy 0.7 | пришёл с работы, уставший",
		"23:00–01:00 | arousal 0.3 | socialEnergy 0.3 | тихо | перед сном",
	}
}

// parsedSchedule — валидное расписание для тестов выбора блока.
func parsedSchedule(t *testing.T) Schedule {
	t.Helper()
	sched, err := parsePersonaSchedule(scheduleText(validScheduleBlocks()...))
	if err != nil {
		t.Fatalf("валидное расписание не разобрано: %v", err)
	}
	if sched == nil {
		t.Fatal("расписание не разобрано: nil")
	}
	return *sched
}

func TestParsePersonaScheduleValid(t *testing.T) {
	sched := parsedSchedule(t)
	if len(sched) != 5 {
		t.Fatalf("блоков %d, хочу 5", len(sched))
	}
	night := sched[0]
	if night.StartMinute != 60 || night.EndMinute != 8*60 {
		t.Fatalf("границы ночного блока %d..%d", night.StartMinute, night.EndMinute)
	}
	if night.Arousal != 0.15 || night.SocialEnergy != 0.1 {
		t.Fatalf("цели ночного блока %+v", night)
	}
	if !night.Quiet || night.Activity != "спит" {
		t.Fatalf("тихий блок разобран неверно: %+v", night)
	}
	if night.Wraps() {
		t.Fatalf("ночной блок 01:00–08:00 не переходит через полночь")
	}
	if night.Line != 2 {
		t.Fatalf("строка ночного блока = %d, хочу 2", night.Line)
	}
	wrapped := sched[4]
	if !wrapped.Wraps() || wrapped.StartMinute != 23*60 || wrapped.EndMinute != 60 {
		t.Fatalf("блок через полночь разобран неверно: %+v", wrapped)
	}
	if wrapped.DurationMinutes() != 120 {
		t.Fatalf("длительность блока через полночь = %d, хочу 120", wrapped.DurationMinutes())
	}
	if wrapped.Line != 6 {
		t.Fatalf("строка вечернего блока = %d, хочу 6", wrapped.Line)
	}
}

// TestParsePersonaScheduleTolerantBits — человекописные мелочи, которые не
// должны валить запуск: дефис/тире вместо «–», запятая в числе, поле «тихо» в
// любом месте строки, двоеточие после имени поля.
func TestParsePersonaScheduleTolerantBits(t *testing.T) {
	sched, err := parsePersonaSchedule(scheduleText(
		"00:00-09:00 | arousal: 0,2 | тихо | socialEnergy: 0,3 | досыпает",
		"09:00—24:00 | socialEnergy 0.7 | arousal 0.6 | дела",
	))
	if err != nil {
		t.Fatalf("терпимые мелочи не разобраны: %v", err)
	}
	if len(*sched) != 2 || (*sched)[0].Arousal != 0.2 || (*sched)[0].SocialEnergy != 0.3 {
		t.Fatalf("числа с запятой/двоеточием разобраны неверно: %+v", (*sched)[0])
	}
	if !(*sched)[0].Quiet || (*sched)[0].Activity != "досыпает" {
		t.Fatalf("блок разобран неверно: %+v", (*sched)[0])
	}
	if (*sched)[1].EndMinute != 1440 || (*sched)[1].Wraps() {
		t.Fatalf("24:00 как конец суток разобран неверно: %+v", (*sched)[1])
	}
}

func TestParsePersonaScheduleNoSection(t *testing.T) {
	sched, err := parsePersonaSchedule("## Имя\nDio\n")
	if err != nil || sched != nil {
		t.Fatalf("без секции хочу nil/nil, получено %v/%v", sched, err)
	}
}

func TestParsePersonaScheduleEmptySection(t *testing.T) {
	_, err := parsePersonaSchedule("## Расписание\n<!-- блоков нет -->\n")
	if err == nil || !strings.Contains(err.Error(), "секция «Расписание» пуста") {
		t.Fatalf("хочу ошибку про пустую секцию, получено: %v", err)
	}
}

// TestParsePersonaScheduleErrors — строгая валидация строки: ошибка называет
// секцию, строку файла и что именно не так.
func TestParsePersonaScheduleErrors(t *testing.T) {
	replaceFirst := func(line string) []string {
		blocks := validScheduleBlocks()
		blocks[0] = line
		return blocks
	}
	cases := []struct {
		name   string
		blocks []string
		want   []string
	}{
		{
			name:   "нет arousal",
			blocks: replaceFirst("01:00–08:00 | socialEnergy 0.1 | тихо | спит"),
			want:   []string{"секция «Расписание»", "строка 2", "arousal"},
		},
		{
			name:   "arousal вне диапазона",
			blocks: replaceFirst("01:00–08:00 | arousal 1.5 | socialEnergy 0.1 | тихо | спит"),
			want:   []string{"строка 2", "arousal должен быть числом от 0 до 1"},
		},
		{
			name:   "два arousal",
			blocks: replaceFirst("01:00–08:00 | arousal 0.2 | arousal 0.3 | socialEnergy 0.1 | спит"),
			want:   []string{"строка 2", "два arousal"},
		},
		{
			name:   "нет socialEnergy",
			blocks: replaceFirst("01:00–08:00 | arousal 0.2 | тихо | спит"),
			want:   []string{"строка 2", "socialEnergy"},
		},
		{
			name:   "нет чем занят",
			blocks: replaceFirst("01:00–08:00 | arousal 0.2 | socialEnergy 0.1 | тихо"),
			want:   []string{"строка 2", "чем занят"},
		},
		{
			name:   "две строки чем занят",
			blocks: replaceFirst("01:00–08:00 | arousal 0.2 | socialEnergy 0.1 | спит | досыпает"),
			want:   []string{"строка 2", "две части"},
		},
		{
			name:   "час вне суток",
			blocks: replaceFirst("25:00–08:00 | arousal 0.2 | socialEnergy 0.1 | тихо | спит"),
			want:   []string{"строка 2", "время", "25:00"},
		},
		{
			name:   "начало совпадает с концом",
			blocks: replaceFirst("08:00–08:00 | arousal 0.2 | socialEnergy 0.1 | тихо | спит"),
			want:   []string{"строка 2", "совпадают"},
		},
		{
			name:   "мусор вместо времени",
			blocks: replaceFirst("утро | arousal 0.2 | socialEnergy 0.1 | тихо | спит"),
			want:   []string{"строка 2", "07:00–09:00"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePersonaSchedule(scheduleText(tc.blocks...))
			if err == nil {
				t.Fatalf("хочу ошибку, файл принят")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("в ошибке нет %q: %v", want, err)
				}
			}
		})
	}
}

// TestParsePersonaScheduleCoverage — блоки обязаны покрывать сутки: дыра и
// перекрытие называются с временем и строкой.
func TestParsePersonaScheduleCoverage(t *testing.T) {
	t.Run("дыра", func(t *testing.T) {
		blocks := validScheduleBlocks()
		blocks[1] = "09:00–11:00 | arousal 0.35 | socialEnergy 0.4 | только проснулся, пьёт кофе"
		_, err := parsePersonaSchedule(scheduleText(blocks...))
		if err == nil || !strings.Contains(err.Error(), "секция «Расписание»") ||
			!strings.Contains(err.Error(), "дыра в расписании 08:00–09:00") {
			t.Fatalf("хочу ошибку про дыру 08:00–09:00, получено: %v", err)
		}
	})

	t.Run("дыра в начале суток", func(t *testing.T) {
		blocks := []string{
			"02:00–24:00 | arousal 0.5 | socialEnergy 0.5 | дела",
		}
		_, err := parsePersonaSchedule(scheduleText(blocks...))
		if err == nil || !strings.Contains(err.Error(), "дыра в расписании 00:00–02:00") {
			t.Fatalf("хочу ошибку про дыру 00:00–02:00, получено: %v", err)
		}
	})

	t.Run("перекрытие", func(t *testing.T) {
		blocks := []string{
			"00:00–12:00 | arousal 0.5 | socialEnergy 0.5 | утро",
			"11:00–24:00 | arousal 0.5 | socialEnergy 0.5 | вечер",
		}
		_, err := parsePersonaSchedule(scheduleText(blocks...))
		for _, want := range []string{"строка 3", "перекрывается", "строки 2", "11:00"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("в ошибке перекрытия нет %q: %v", want, err)
			}
		}
	})

	t.Run("перекрытие через полночь", func(t *testing.T) {
		blocks := []string{
			"00:00–03:00 | arousal 0.5 | socialEnergy 0.5 | ночь",
			"03:00–22:00 | arousal 0.5 | socialEnergy 0.5 | день",
			"22:00–02:00 | arousal 0.5 | socialEnergy 0.5 | вечер",
		}
		_, err := parsePersonaSchedule(scheduleText(blocks...))
		if err == nil || !strings.Contains(err.Error(), "перекрывается") ||
			!strings.Contains(err.Error(), "00:00") {
			t.Fatalf("хочу ошибку перекрытия через полночь, получено: %v", err)
		}
	})
}

// TestScheduleBlockAt — блок выбирается по местным часам: начало входит,
// конец нет, блок через полночь живёт после 00:00.
func TestScheduleBlockAt(t *testing.T) {
	sched := parsedSchedule(t)
	loc := moscow(t)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 3, h, m, 0, 0, loc) }
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"ночь", at(3, 0), "спит"},
		{"начало блока входит", at(8, 0), "только проснулся, пьёт кофе"},
		{"последняя минута блока", at(10, 59), "только проснулся, пьёт кофе"},
		{"день", at(12, 0), "на работе"},
		{"вечер", at(20, 0), "пришёл с работы, уставший"},
		{"начало блока через полночь", at(23, 0), "перед сном"},
		{"после полуночи блок вчерашний", at(0, 30), "перед сном"},
		{"последняя минута перед сном", at(0, 59), "перед сном"},
		{"ночной блок с 01:00", at(1, 0), "спит"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			block := sched.BlockAt(tc.when, loc)
			if block == nil {
				t.Fatalf("блок не найден")
			}
			if block.Activity != tc.want {
				t.Fatalf("блок = %q, хочу %q", block.Activity, tc.want)
			}
		})
	}
}

// TestScheduleBlockAtTimezone — час берётся в поясе персонажа (AGENT_TIMEZONE):
// один и тот же момент — разные блоки в UTC и Москве.
func TestScheduleBlockAtTimezone(t *testing.T) {
	sched := parsedSchedule(t)
	loc := moscow(t)
	instant := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC) // 12:00 МСК
	if block := sched.BlockAt(instant, time.UTC); block == nil || block.Activity != "только проснулся, пьёт кофе" {
		t.Fatalf("в UTC 09:00 ожидал утро, получено %+v", block)
	}
	if block := sched.BlockAt(instant, loc); block == nil || block.Activity != "на работе" {
		t.Fatalf("в Москве 12:00 ожидал работу, получено %+v", block)
	}
}

// TestParsePersonaFileSchedule — секция проходит через общий парсер persona:
// есть — расписание собрано, нет — прежнее поведение (nil).
func TestParsePersonaFileSchedule(t *testing.T) {
	text := validPersonaText() + scheduleText(validScheduleBlocks()...)
	src, err := ParsePersonaFile(text)
	if err != nil {
		t.Fatalf("persona с расписанием не разобрана: %v", err)
	}
	if src.Schedule == nil || len(*src.Schedule) != 5 {
		t.Fatalf("расписание не попало в источник персоны: %+v", src.Schedule)
	}

	src, err = ParsePersonaFile(validPersonaText())
	if err != nil {
		t.Fatalf("persona без расписания не разобрана: %v", err)
	}
	if src.Schedule != nil {
		t.Fatalf("без секции расписание не должно появляться: %+v", src.Schedule)
	}
}

// TestShippedPersonaSchedule — shipped-файл: ночью тихо и сон, днём тишины нет.
func TestShippedPersonaSchedule(t *testing.T) {
	src, err := ParsePersonaFile(readRepoFile(t, "../../config/persona.md"))
	if err != nil {
		t.Fatalf("config/persona.md не парсится: %v", err)
	}
	if src.Schedule == nil {
		t.Fatal("в config/persona.md нет расписания дня")
	}
	loc := moscow(t)
	if block := src.Schedule.BlockAt(time.Date(2026, 11, 3, 3, 0, 0, 0, loc), loc); block == nil || !block.Quiet {
		t.Fatalf("ночью ожидал тихий блок, получено %+v", block)
	}
	if block := src.Schedule.BlockAt(time.Date(2026, 11, 3, 12, 0, 0, 0, loc), loc); block == nil || block.Quiet {
		t.Fatalf("днём ожидал бодрствующий блок, получено %+v", block)
	}
}
