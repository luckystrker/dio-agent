// Расписание дня персонажа (этап 22, решение владельца 2026-11-03): живая
// секция «Расписание» в файле персоны — блоки по часам с целевыми arousal и
// socialEnergy и строкой «чем занят». Настроение плавно тянется к блоку тем же
// механизмом затухания (§7.2), панель состояния получает строку «сейчас: …,
// чем занят», а тихий блок переносит инициативы фона на своё окончание (§14.3).
//
// Секция необязательна: нет её — прежнее поведение (базовая линия настроения,
// инициативы без тихих часов). Расписание — настройка процесса, как каркас
// промпта: в версию персоны в БД не входит, правка файла применяется
// перезапуском службы (§7.1).
package character

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// scheduleSection — имя секции расписания дня в persona-файле.
const scheduleSection = "Расписание"

// minutesPerDay — минут в сутках: расписание покрывает их все.
const minutesPerDay = 24 * 60

// scheduleLineLimit — потолок строки блока: расписание — короткая заметка, а
// не абзац (лишнее — почти наверняка забытая склейка строк).
const scheduleLineLimit = 500

// scheduleRangePattern — «07:00–09:00»: час в сутки (0..24), минуты 0..59,
// разделитель — дефис, тире или длинное тире.
var scheduleRangePattern = regexp.MustCompile(`^(\d{1,2}):(\d{2})\s*[–—-]\s*(\d{1,2}):(\d{2})$`)

// scheduleFieldPattern — «arousal 0.3» / «arousal: 0.3».
var scheduleFieldPattern = regexp.MustCompile(`^(arousal|socialEnergy)\s*:?\s*(\S+)$`)

// scheduleQuietToken — пометка тихого блока.
const scheduleQuietToken = "тихо"

// ScheduleBlock — один блок расписания дня: с какого по какое время (местные
// часы AGENT_TIMEZONE), целевые arousal/socialEnergy и «чем занят».
type ScheduleBlock struct {
	StartMinute  int     // 0..1439
	EndMinute    int     // 1..1440; EndMinute <= StartMinute — блок через полночь
	Arousal      float64 // 0..1 — к какому уровню энергии тянется настроение
	SocialEnergy float64 // 0..1 — к какому желанию общаться тянется настроение
	Activity     string  // «чем занят» одной строкой (в панель состояния)
	Quiet        bool    // тихий блок: инициативы ждут его окончания (§14.3)
	Line         int     // строка файла — для сообщений об ошибках
}

// Wraps — блок переходит через полночь (23:00–01:00).
func (b ScheduleBlock) Wraps() bool { return b.EndMinute <= b.StartMinute }

// DurationMinutes — длина блока в минутах (через полночь — до 24 часов).
func (b ScheduleBlock) DurationMinutes() int {
	if b.EndMinute > b.StartMinute {
		return b.EndMinute - b.StartMinute
	}
	return minutesPerDay - b.StartMinute + b.EndMinute
}

// contains — входит ли минута суток в блок (начало входит, конец нет).
func (b ScheduleBlock) contains(minute int) bool {
	if b.Wraps() {
		return minute >= b.StartMinute || minute < b.EndMinute
	}
	return minute >= b.StartMinute && minute < b.EndMinute
}

// Schedule — расписание дня: блоки покрывают сутки без дырок и перекрытий
// (проверяет парсер).
type Schedule []ScheduleBlock

// BlockAt — блок, действующий в момент at по часам loc (nil — пояс персонажа
// по умолчанию). nil — расписания нет или момент не покрыт (парсер такого не
// пропускает).
func (s Schedule) BlockAt(at time.Time, loc *time.Location) *ScheduleBlock {
	minute := minuteOfDay(at, loc)
	for i := range s {
		if s[i].contains(minute) {
			return &s[i]
		}
	}
	return nil
}

// QuietUntil — если в момент at действует тихий блок, возвращает момент, когда
// тишина кончится (абсолютное время, уже в поясе loc). Второе значение —
// «сейчас тихо».
func (s Schedule) QuietUntil(at time.Time, loc *time.Location) (time.Time, bool) {
	block := s.BlockAt(at, loc)
	if block == nil || !block.Quiet {
		return time.Time{}, false
	}
	zone := locationOrDefault(loc)
	local := at.In(zone)
	hour, minute := block.EndMinute/60, block.EndMinute%60
	// Конец блока через полночь — на следующий день. time.Date нормализует
	// hour=24 в 00:00 следующего дня и корректно обходит переходы пояса.
	day := local.Day()
	if block.Wraps() {
		day++
	}
	return time.Date(local.Year(), local.Month(), day, hour, minute, 0, 0, zone), true
}

// MoodTargetAt — цель затухания настроения (этап 22.2): valence остаётся
// базовой линией персоны, arousal и socialEnergy берутся из блока текущего
// времени. Расписания нет — прежняя базовая линия.
func (p Persona) MoodTargetAt(at time.Time, loc *time.Location) Mood {
	if p.Schedule == nil {
		return p.MoodBaseline
	}
	block := p.Schedule.BlockAt(at, loc)
	if block == nil {
		return p.MoodBaseline
	}
	return Mood{
		Valence:      p.MoodBaseline.Valence,
		Arousal:      block.Arousal,
		SocialEnergy: block.SocialEnergy,
	}
}

// QuietUntil — тихий блок расписания персоны в момент at (§14.3, этап 22.4).
func (p Persona) QuietUntil(at time.Time, loc *time.Location) (time.Time, bool) {
	if p.Schedule == nil {
		return time.Time{}, false
	}
	return p.Schedule.QuietUntil(at, loc)
}

// ActivityAt — «чем занят» по расписанию в момент at (пусто — расписания нет).
func (p Persona) ActivityAt(at time.Time, loc *time.Location) string {
	if p.Schedule == nil {
		return ""
	}
	if block := p.Schedule.BlockAt(at, loc); block != nil {
		return block.Activity
	}
	return ""
}

// minuteOfDay — минута суток в поясе персонажа (AGENT_TIMEZONE; nil — пояс
// по умолчанию, как у панели).
func minuteOfDay(at time.Time, loc *time.Location) int {
	local := at.In(locationOrDefault(loc))
	return local.Hour()*60 + local.Minute()
}

// numberedLine — строка файла с номером: ошибка расписания обязана называть
// строку, которую человек видит в редакторе.
type numberedLine struct {
	number int
	text   string
}

// parsePersonaSchedule разбирает секцию «Расписание» из текста persona-файла.
// nil/nil — секции нет (прежнее поведение). Ошибка называет секцию, строку и
// что именно не так.
func parsePersonaSchedule(text string) (*Schedule, error) {
	lines, found := scheduleSectionLines(text)
	if !found {
		return nil, nil
	}
	var (
		schedule Schedule
		cover    [minutesPerDay]int // 0 — минута не покрыта, иначе строка блока
	)
	for _, nl := range lines {
		raw := strings.TrimSpace(nl.text)
		if raw == "" {
			continue
		}
		block, err := parseScheduleBlock(raw, nl.number)
		if err != nil {
			return nil, err
		}
		for m := 0; m < block.DurationMinutes(); m++ {
			minute := (block.StartMinute + m) % minutesPerDay
			if prev := cover[minute]; prev != 0 {
				return nil, fmt.Errorf(
					"секция «%s», строка %d: блок «%s» перекрывается с блоком строки %d в %s",
					scheduleSection, block.Line, raw, prev, formatMinute(minute))
			}
			cover[minute] = block.Line
		}
		schedule = append(schedule, block)
	}
	if len(schedule) == 0 {
		return nil, fmt.Errorf(
			"секция «%s» пуста — нужен хотя бы один блок расписания или удалите секцию", scheduleSection)
	}
	if start, end, ok := firstUncovered(cover); !ok {
		return nil, fmt.Errorf(
			"секция «%s»: дыра в расписании %s–%s — блоки должны покрывать сутки целиком",
			scheduleSection, formatMinute(start), formatMinute(end))
	}
	return &schedule, nil
}

// parseScheduleBlock разбирает одну строку: время, поля arousal/socialEnergy,
// пометку «тихо» и строку «чем занят». Порядок полей свободный.
func parseScheduleBlock(raw string, line int) (ScheduleBlock, error) {
	where := fmt.Sprintf("секция «%s», строка %d", scheduleSection, line)
	if runes := []rune(raw); len(runes) > scheduleLineLimit {
		return ScheduleBlock{}, fmt.Errorf("%s: строка блока длиннее %d символов — расписание должно читаться глазом",
			where, scheduleLineLimit)
	}
	fields := strings.Split(raw, "|")
	if len(fields) < 3 {
		return ScheduleBlock{}, fmt.Errorf(
			"%s: блок «%s» не разобран — формат: 07:00–09:00 | arousal 0.3 | socialEnergy 0.3 | чем занят",
			where, raw)
	}
	start, end, err := parseScheduleRange(strings.TrimSpace(fields[0]), where, raw)
	if err != nil {
		return ScheduleBlock{}, err
	}
	block := ScheduleBlock{StartMinute: start, EndMinute: end, Line: line}
	var (
		hasArousal  bool
		hasSocial   bool
		hasActivity bool
	)
	for _, field := range fields[1:] {
		field = strings.TrimSpace(field)
		switch {
		case field == "":
			// хвостовой разделитель — не ошибка
		case strings.EqualFold(field, scheduleQuietToken):
			block.Quiet = true
		default:
			match := scheduleFieldPattern.FindStringSubmatch(field)
			if match == nil {
				if hasActivity {
					return ScheduleBlock{}, fmt.Errorf(
						"%s: две части «чем занят» («%s» и «%s») — в блоке ровно одна строка занятия",
						where, block.Activity, field)
				}
				block.Activity, hasActivity = field, true
				continue
			}
			name, value := match[1], match[2]
			parsed, perr := strconv.ParseFloat(strings.ReplaceAll(value, ",", "."), 64)
			if perr != nil || parsed < 0 || parsed > 1 {
				return ScheduleBlock{}, fmt.Errorf(
					"%s: %s должен быть числом от 0 до 1, получено: %s", where, name, value)
			}
			if name == "arousal" {
				if hasArousal {
					return ScheduleBlock{}, fmt.Errorf("%s: два arousal — в блоке должен быть один", where)
				}
				block.Arousal, hasArousal = parsed, true
			} else {
				if hasSocial {
					return ScheduleBlock{}, fmt.Errorf("%s: два socialEnergy — в блоке должен быть один", where)
				}
				block.SocialEnergy, hasSocial = parsed, true
			}
		}
	}
	switch {
	case !hasArousal:
		return ScheduleBlock{}, fmt.Errorf(
			"%s: нет arousal (0..1) — блок «%s»", where, raw)
	case !hasSocial:
		return ScheduleBlock{}, fmt.Errorf(
			"%s: нет socialEnergy (0..1) — блок «%s»", where, raw)
	case !hasActivity:
		return ScheduleBlock{}, fmt.Errorf(
			"%s: нет части «чем занят» — что персонаж делает в этот блок: «%s»", where, raw)
	}
	return block, nil
}

// parseScheduleRange разбирает «07:00–09:00» в минуты суток. Конец 24:00 —
// полночь. Начало и конец не могут совпадать: блок нулевой длины бессмыслен, а
// «через полночь» записывается как 23:00–01:00.
func parseScheduleRange(raw, where, block string) (int, int, error) {
	match := scheduleRangePattern.FindStringSubmatch(raw)
	if match == nil {
		return 0, 0, fmt.Errorf(
			"%s: время «%s» не разобрано — нужно «07:00–09:00» (конец суток — 24:00)", where, raw)
	}
	toInt := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	startHour, startMinute := toInt(match[1]), toInt(match[2])
	endHour, endMinute := toInt(match[3]), toInt(match[4])
	badTime := func() error {
		return fmt.Errorf("%s: время «%s» вне суток — час 0..23, конец блока до 24:00", where, raw)
	}
	if startHour > 23 || startMinute > 59 || endHour > 24 || endMinute > 59 {
		return 0, 0, badTime()
	}
	if endHour == 24 && endMinute != 0 {
		return 0, 0, badTime()
	}
	start, end := startHour*60+startMinute, endHour*60+endMinute
	if start == end {
		return 0, 0, fmt.Errorf(
			"%s: начало и конец блока совпадают (%s) — блок нулевой длины в «%s»", where, raw, block)
	}
	return start, end, nil
}

// firstUncovered — первый непокрытый промежуток суток [start,end) в минутах;
// ok=true — покрыто всё.
func firstUncovered(cover [minutesPerDay]int) (int, int, bool) {
	for m := 0; m < minutesPerDay; m++ {
		if cover[m] != 0 {
			continue
		}
		end := m
		for end < minutesPerDay && cover[end] == 0 {
			end++
		}
		return m, end, false
	}
	return 0, 0, true
}

// formatMinute — «07:05» (1440 — «24:00»).
func formatMinute(minute int) string {
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}

// scheduleSectionLines — строки секции «Расписание» ИСХОДНОГО файла с номерами:
// HTML-комментарии пропускаются, но нумерация не сбивается. Первое вхождение
// заголовка выигрывает (как splitSections), секция кончается на следующем «## ».
func scheduleSectionLines(text string) ([]numberedLine, bool) {
	var (
		lines     []numberedLine
		inSection bool
		found     bool
		inComment bool
	)
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if inComment {
			if strings.Contains(line, "-->") {
				inComment = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "<!--") {
			if !strings.Contains(trimmed, "-->") {
				inComment = true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			header := strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			switch {
			case header == scheduleSection && !found:
				inSection, found = true, true
			case inSection:
				return lines, true
			}
			continue
		}
		if inSection {
			lines = append(lines, numberedLine{number: i + 1, text: line})
		}
	}
	return lines, found
}
