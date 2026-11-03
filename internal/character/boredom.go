// Скука (этап 23.1, решение владельца 2026-11-03, §8): внутреннее давление,
// которое растёт со временем без событий и толкает к ВНУТРЕННЕЙ активности
// (поиск интереса), но само сообщений не порождает — написать первым можно
// только share-намерением по зацепившей находке (§14.3).
//
// Формула детерминированная и не требует модели: за BoredomFullHours часов
// бодрствования без событий шкала доходит до 1. В тихих блоках расписания
// (этап 22.4) не растёт. События сбрасывают: разговорный ход — сильно вниз
// (до нуля), применённый explore — заметно вниз. Время — параметр, поэтому
// поведение проверяется юнит-тестами от фиксированных часов.
package character

import (
	"context"
	"fmt"
	"time"
)

const (
	// BoredomFullHours — часов бодрствования без событий до полной скуки.
	// Крутится здесь: меньше — персонаж скучает быстрее.
	BoredomFullHours = 7.0
	// BoredomGrowthPerHour — линейный рост шкалы за час бодрствования.
	BoredomGrowthPerHour = 1.0 / BoredomFullHours
	// BoredomThreshold — порог, после которого фон ищет интерес (§14.3).
	// Крутится здесь: ниже — персонаж чаще уходит в собственные темы.
	BoredomThreshold = 0.6
	// BoredomExploreRelief — насколько применённый explore сбрасывает скуку.
	BoredomExploreRelief = 0.5
	// boredomWriteEps — порог заметности роста: чтение не пишет строку на
	// каждый тик (как moodDecayWriteEps у настроения).
	boredomWriteEps = 0.01
)

// BoredomEvent — событие, сбрасывающее скуку.
type BoredomEvent int

const (
	// BoredomConversation — состоявшийся разговорный ход: сброс до нуля.
	BoredomConversation BoredomEvent = iota
	// BoredomExplore — применённая находка explore: заметный спад.
	BoredomExplore
)

// GrowBoredom — чистая формула роста: stored плюс линейный рост за время
// бодрствования [from, to], без тихих блоков; потолок 1.
func GrowBoredom(stored float64, from, to time.Time, schedule *Schedule, loc *time.Location) float64 {
	stored = clamp(stored, 0, 1)
	if !to.After(from) {
		return stored
	}
	awake := AwakeDuration(from, to, schedule, loc)
	return clamp(stored+awake.Hours()*BoredomGrowthPerHour, 0, 1)
}

// AwakeDuration — сколько времени из [from, to] персонаж бодрствует по
// расписанию (тихие блоки не в счёт). Расписания нет — всё время бодрствование
// (прежнее поведение без ритма дня).
func AwakeDuration(from, to time.Time, schedule *Schedule, loc *time.Location) time.Duration {
	if !to.After(from) {
		return 0
	}
	if schedule == nil || len(*schedule) == 0 {
		return to.Sub(from)
	}
	zone := locationOrDefault(loc)
	local := from.In(zone)
	// Идём по локальным суткам: блоки заданы минутами суток, тихий блок через
	// полночь (23:00–01:00) попадает хвостом в следующий день.
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	var awake time.Duration
	for !day.After(to) {
		for _, block := range *schedule {
			if block.Quiet {
				continue
			}
			// Оба конца — по стенным часам пояса (time.Date сам разбирается с
			// переходами): расписание задаёт часы суток, а не длительность;
			// в день перевода стрелок бодрствование 01:00–08:00 физически
			// короче/дольше. Конец через полночь — на следующие сутки.
			start := time.Date(day.Year(), day.Month(), day.Day(),
				block.StartMinute/60, block.StartMinute%60, 0, 0, zone)
			endDay := day
			if block.Wraps() {
				endDay = day.AddDate(0, 0, 1)
			}
			end := time.Date(endDay.Year(), endDay.Month(), endDay.Day(),
				block.EndMinute/60, block.EndMinute%60, 0, 0, zone)
			if end.Before(from) || start.After(to) {
				continue
			}
			if start.Before(from) {
				start = from
			}
			if end.After(to) {
				end = to
			}
			if end.After(start) {
				awake += end.Sub(start)
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return awake
}

// boredomAfterEvent — значение шкалы после события.
func boredomAfterEvent(current float64, event BoredomEvent) float64 {
	switch event {
	case BoredomConversation:
		return 0
	case BoredomExplore:
		return clamp(current-BoredomExploreRelief, 0, 1)
	default:
		return clamp(current, 0, 1)
	}
}

// BoredomRepo — шкала скуки в character_state (singleton id=1, миграция 000016).
type BoredomRepo struct{}

// NewBoredomRepo создаёт репозиторий (без состояния — методы принимают DBTX).
func NewBoredomRepo() *BoredomRepo { return &BoredomRepo{} }

// ReadGrown читает скуку, доросшую до now, и фиксирует её в хранилище — но не
// вслепую: запись только когда рост заметен (boredomWriteEps) и строку с
// момента чтения никто не трогал (CAS по boredom_updated_at). baseline —
// текущая цель настроения (блок расписания/база персоны): при создании строки
// она попадает в mood-колонки, иначе дефолты таблицы подменили бы настроение.
func (r *BoredomRepo) ReadGrown(ctx context.Context, db DBTX, now time.Time,
	schedule *Schedule, loc *time.Location, baseline Mood) (float64, error) {
	var (
		stored float64
		from   time.Time
	)
	err := db.QueryRow(ctx,
		`SELECT boredom, boredom_updated_at FROM character_state WHERE id = 1`,
	).Scan(&stored, &from)
	if isNoRows(err) {
		// Строки ещё нет (до первого применения настроения): скука стартует
		// с нуля, а настроение — с текущей цели персоны/расписания. Иначе
		// первый же разговор считал бы дельту от дефолтов 0/0.4/0.6.
		if _, ierr := db.Exec(ctx, `
			INSERT INTO character_state
			  (id, persona_version, valence, arousal, social_energy, boredom, boredom_updated_at)
			VALUES (1, (SELECT COALESCE(MAX(version), 1) FROM personas WHERE active = TRUE), $1, $2, $3, 0, $4)
			ON CONFLICT (id) DO NOTHING`,
			baseline.Valence, baseline.Arousal, baseline.SocialEnergy, now); ierr != nil {
			return 0, fmt.Errorf("скука: строка состояния не создана: %w", ierr)
		}
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("скука: значение не прочитано: %w", err)
	}
	grown := GrowBoredom(stored, from, now, schedule, loc)
	if grown-stored <= boredomWriteEps {
		return grown, nil
	}
	var written float64
	err = db.QueryRow(ctx,
		`UPDATE character_state SET boredom = $1, boredom_updated_at = $2
		 WHERE id = 1 AND boredom_updated_at = $3
		 RETURNING boredom`,
		grown, now, from).Scan(&written)
	if isNoRows(err) {
		// Пока считали, шкалу сбросил разговор/находка: наше grown устарело.
		// Перечитываем актуальное значение и доросшее от него — не возвращаем
		// завышенную скуку (иначе триггер сработал бы сразу после разговора).
		var (
			current float64
			at      time.Time
		)
		if rerr := db.QueryRow(ctx,
			`SELECT boredom, boredom_updated_at FROM character_state WHERE id = 1`,
		).Scan(&current, &at); rerr != nil {
			return 0, fmt.Errorf("скука: актуальное значение не перечитано: %w", rerr)
		}
		return GrowBoredom(current, at, now, schedule, loc), nil
	}
	if err != nil {
		return 0, fmt.Errorf("скука: доросшее значение не записано: %w", err)
	}
	return written, nil
}

// ApplyEvent применяет событие ВНУТРИ транзакции применения: значение
// дорастает до now (с учётом тихих блоков) и падает по правилу события.
// SELECT ... FOR UPDATE сериализует параллельные применения. baseline — цель
// настроения для вставки недостающей строки (как в ReadGrown).
func (r *BoredomRepo) ApplyEvent(ctx context.Context, db DBTX, event BoredomEvent,
	now time.Time, schedule *Schedule, loc *time.Location, baseline Mood) (float64, error) {
	var (
		stored float64
		from   time.Time
	)
	err := db.QueryRow(ctx,
		`SELECT boredom, boredom_updated_at FROM character_state WHERE id = 1 FOR UPDATE`,
	).Scan(&stored, &from)
	switch {
	case isNoRows(err):
		stored, from = 0, now
	case err != nil:
		return 0, fmt.Errorf("скука: чтение перед событием не удалось: %w", err)
	}
	grown := GrowBoredom(stored, from, now, schedule, loc)
	next := round3(boredomAfterEvent(grown, event))
	// Вставка (строки ещё нет) берёт текущую цель настроения: дефолты таблицы
	// не должны подменять базовую линию (как в ReadGrown).
	if _, err := db.Exec(ctx, `
		INSERT INTO character_state
		  (id, persona_version, valence, arousal, social_energy, boredom, boredom_updated_at)
		VALUES (1, (SELECT COALESCE(MAX(version), 1) FROM personas WHERE active = TRUE), $1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE
		  SET boredom = EXCLUDED.boredom, boredom_updated_at = EXCLUDED.boredom_updated_at`,
		baseline.Valence, baseline.Arousal, baseline.SocialEnergy, next, now); err != nil {
		return 0, fmt.Errorf("скука: событие не применено: %w", err)
	}
	return next, nil
}
