// Панель состояния хода (решение 2026-11-02): служебный блок, который
// харнесс кладёт ПЕРВОЙ частью финального пользовательского сообщения хода —
// перед текстом пачки. Здесь всё, что меняется между ходами: текущее время,
// пауза с прошлого сообщения, собеседник, настроение, отношение и карточка
// памяти. System-промпт остаётся статичным байт-в-байт, поэтому кэш префикса
// провайдера не ломается при смене настроения.
//
// Панель не пишется в БД и в историю следующих ходов не попадает: история
// строится из social_events/delivery_outbox, а панель живёт только в запросе
// текущего хода.
package character

import (
	"fmt"
	"strings"
	"time"
)

// Маркеры панели: модель явно видит, что это данные харнесса, а не реплика.
const (
	statePanelOpen  = "[состояние хода — служебное, не от собеседника]"
	statePanelClose = "[/состояние]"
)

// StatePanelMarkers — открывающий и закрывающий маркеры панели состояния.
// Нужны replay-абляции no-panel (40.2): пакет modebug не импортирует
// character (character импортирует modebug), поэтому строки отдаются наружу.
func StatePanelMarkers() (open, close string) { return statePanelOpen, statePanelClose }

// weekdayShort — русские сокращения дней недели в порядке time.Weekday.
var weekdayShort = [...]string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}

// defaultLocation — пояс по умолчанию, если конфиг его не задал (тесты и
// частичная сборка): Europe/Moscow, затем UTC как последний фолбэк (в бинарник
// зоны встроены через time/tzdata).
var defaultLocation = func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Moscow"); err == nil {
		return loc
	}
	return time.UTC
}()

// locationOrDefault — пояс меток времени: nil — пояс по умолчанию.
func locationOrDefault(loc *time.Location) *time.Location {
	if loc == nil {
		return defaultLocation
	}
	return loc
}

// formatHistoryTime — абсолютная метка реплики в часовом поясе персонажа:
// «пн 02.11 17:02». Абсолютная (не «2 часа назад») — история обязана быть
// байт-стабильной между ходами, иначе ломается кэш префикса.
func formatHistoryTime(at time.Time, loc *time.Location) string {
	t := at.In(locationOrDefault(loc))
	return fmt.Sprintf("%s %s %s", weekdayShort[t.Weekday()], t.Format("02.01"), t.Format("15:04"))
}

// describePause — человеческая длительность паузы без относительных формул в
// истории: «2 ч 14 мин», «14 мин», «3 дн 4 ч», «меньше минуты».
func describePause(d time.Duration) string {
	if d < time.Minute {
		return "меньше минуты"
	}
	minutes := int(d / time.Minute)
	hours := minutes / 60
	days := hours / 24
	switch {
	case days > 0:
		h := hours % 24
		if h == 0 {
			return fmt.Sprintf("%d дн", days)
		}
		return fmt.Sprintf("%d дн %d ч", days, h)
	case hours > 0:
		m := minutes % 60
		if m == 0 {
			return fmt.Sprintf("%d ч", hours)
		}
		return fmt.Sprintf("%d ч %d мин", hours, m)
	default:
		return fmt.Sprintf("%d мин", minutes)
	}
}

// statePanelData — содержимое панели; пустые строки не выводятся.
type statePanelData struct {
	Now          time.Time
	Location     *time.Location
	LastAt       time.Time // время последней реплики истории; zero — истории нет
	HasLast      bool
	Actor        string
	Mood         string
	Relationship string
	// Activity — «чем занят» по расписанию дня (этап 22.3): дописывается к
	// строке «сейчас: <время>». Пусто — расписания нет.
	Activity   string
	MemoryCard string
	// Stickers — краткий список коллекции стикеров (этап 25.3/25.4): метка,
	// эмодзи и заметка «когда уместен»; по метке модель просит стикер.
	Stickers string
	// SaveSticker — подсказка «можно сохранить» (28.1): печатается только
	// когда в пачке есть стикер, которого нет в коллекции. Без этого модель
	// предлагала save_sticker в каждом ходе, в том числе без стикера вовсе.
	SaveSticker bool
	// MemoryTools — инструменты памяти выданы на этом ходе (этап 21.3):
	// подсказка прямо в панели, что это страховка, а не основной путь.
	MemoryTools bool
	MultiActor  bool
	// GroupInvolved — чат вовлечён (30.2): бот уже высказался, сейчас можно
	// отвечать или молчать без обязательства.
	GroupInvolved bool
	// InitiativesExhausted — дневной лимит инициатив личного чата исчерпан
	// (§14.3, этап 33.2): модель видит, что check_in/share планировать нельзя.
	InitiativesExhausted bool
	// WebTools — веб-инструменты выданы на этом ходе (36.1): подсказка, что
	// идти в веб стоит только за свежим фактом.
	WebTools bool
	// WebExhausted — суточный лимит поисков разговорного пула исчерпан (36.1):
	// инструменты объявлены, но вызов отклоняется лимитом (39.2) — панель
	// прямо говорит, что искать не надо.
	WebExhausted bool
	// FieldHints — подсказки о применимых на этом ходе полях TurnResult
	// (39.2): идут в панель (последнее user-сообщение), чтобы system, схема и
	// набор инструментов оставались стабильным префиксом запроса.
	FieldHints []string
}

// sanitizePanelValue — одна строка панели из внешнего/изменяемого значения
// (имя собеседника приходит из Telegram-профиля): переводы строк схлопываются,
// квадратные скобки заменяются скобками (нельзя подделать маркер панели),
// хвост обрезается. Данные не должны уметь менять структуру служебного блока.
func sanitizePanelValue(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	value = strings.NewReplacer("[", "(", "]", ")").Replace(value)
	if limit > 0 {
		if runes := []rune(value); len(runes) > limit {
			value = string(runes[:limit-1]) + "…"
		}
	}
	return strings.TrimSpace(value)
}

// buildStatePanel собирает служебный блок. Порядок строк фиксирован, панель
// детерминирована при одинаковом состоянии.
func buildStatePanel(d statePanelData) string {
	lines := []string{statePanelOpen}
	// «сейчас: пн 02.11 17:02, на работе» — время и занятие по расписанию дня
	// одной строкой (этап 22.3): модель видит, чем персонаж занят в этот час.
	nowLine := "сейчас: " + formatHistoryTime(d.Now, d.Location)
	if activity := sanitizePanelValue(d.Activity, 200); activity != "" {
		nowLine += ", " + activity
	}
	lines = append(lines, nowLine)
	if d.HasLast {
		pause := d.Now.Sub(d.LastAt)
		if pause < 0 {
			pause = 0
		}
		lines = append(lines, "с прошлого сообщения в чате: "+describePause(pause))
	}
	if actor := sanitizePanelValue(d.Actor, 200); actor != "" {
		lines = append(lines, "ты говоришь с: "+actor)
	}
	if mood := sanitizePanelValue(d.Mood, 300); mood != "" {
		lines = append(lines, "твоё состояние: "+mood)
	}
	if rel := sanitizePanelValue(d.Relationship, 300); rel != "" {
		lines = append(lines, "отношение к собеседнику: "+rel)
	}
	if card := strings.TrimSpace(d.MemoryCard); card != "" {
		lines = append(lines, card)
	}
	if stickers := strings.TrimSpace(d.Stickers); stickers != "" {
		lines = append(lines, stickers)
	}
	if d.SaveSticker {
		lines = append(lines, "в пачке стикер, которого нет в твоей коллекции — можно сохранить его себе (save_sticker с заметкой, когда уместен)")
	}
	if d.MemoryTools {
		lines = append(lines, "память доступна инструментами — обращаться только если ответа нет ни в истории, ни в карточке")
	}
	if d.MultiActor {
		lines = append(lines, "в пачке несколько участников — отвечай на пачку целиком")
	}
	if d.GroupInvolved {
		lines = append(lines, "ты в разговоре группы — можешь ответить, поставить реакцию или промолчать")
	}
	if d.InitiativesExhausted {
		lines = append(lines, "на сегодня инициатив больше нельзя — не планируй check_in/share; отложить ответ (defer) можно")
	}
	if d.WebTools && !d.WebExhausted {
		lines = append(lines, "веб доступен инструментами (поиск и web_fetch) — только если нужен свежий факт, которого нет в истории и памяти, или тебя прямо просят поискать/найти/скинуть")
	}
	if d.WebExhausted {
		lines = append(lines, "поиск на сегодня исчерпан — веб-инструменты объявлены, но вызов отклоняется лимитом; отвечай без поиска")
	}
	// Применимые поля TurnResult (39.2) — в панели, а не в system/схеме.
	for _, hint := range d.FieldHints {
		if hint = strings.TrimSpace(hint); hint != "" {
			lines = append(lines, hint)
		}
	}
	lines = append(lines, statePanelClose)
	return strings.Join(lines, "\n")
}

// composeUserText — финальное пользовательское сообщение хода: сначала
// служебная панель, затем текст пачки (фото уходят отдельными частями Ask).
func composeUserText(panel, batch string) string {
	panel = strings.TrimSpace(panel)
	batch = strings.TrimSpace(batch)
	switch {
	case panel == "":
		return batch
	case batch == "":
		return panel
	default:
		return panel + "\n\n" + batch
	}
}

// ComposeUserText — то же для фоновых задач (28.4): панель первой частью
// user-сообщения, как в разговорном ходе.
func ComposeUserText(panel, body string) string { return composeUserText(panel, body) }

// BackgroundPanel — компактная панель состояния фоновой задачи (28.4): время
// и «чем занят» по расписанию, настроение, собеседник и отношение к нему (если
// есть). Собирается тем же buildStatePanel, что и разговорный ход, но без
// Assemble: у фона нет пачки, истории и карточки памяти. Фон кладёт панель
// ПЕРВОЙ частью user-сообщения — иначе шаблон system её не видит (shipped
// шаблон динамику игнорирует, решения 2026-11-02/2026-11-03).
func BackgroundPanel(now time.Time, loc *time.Location, activity, actor, mood, relationship string) string {
	return buildStatePanel(statePanelData{
		Now:          now,
		Location:     loc,
		Activity:     activity,
		Actor:        actor,
		Mood:         mood,
		Relationship: relationship,
	})
}
