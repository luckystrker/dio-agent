// Сборка базового контекста хода (§11.1): то, что приложение собирает всегда,
// независимо от поведения модели. Пункты §11.1:
//  1. core persona + runtime constraints (constraints — отдельная структурная
//     секция контекста, не склеенная с недоверенными данными, §17);
//  2. текущий batch (цель фоновой работы приходит в 9.6);
//  3. краткое текущее mood;
//  4. разрешённые отношения с участниками;
//  5. последние сообщения чата, обе стороны (окно и потолок объёма из
//     конфигурации), только доставленные ответы;
//  6. панель состояния (panel.go) — время, пауза, собеседник, состояние,
//     отношение и карточка памяти собеседника (решение 2026-11-02).
//
// System-промпт содержит только статику: динамика (3, 4, 6) идёт в панель
// состояния — первой частью финального пользовательского сообщения, поэтому
// system байт-в-байт одинаков между ходами одного чата (кэш префикса
// провайдера). Карточка памяти (п.6) собирается харнессом заранее только на
// разговорном ходе; при урезанной истории или не влезшей карточке модель
// получает инструменты памяти (решение 2026-11-02, §10.4).
package character

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/telegram"
)

// BatchEvent — входное событие пачки, попавшее в ход.
type BatchEvent struct {
	EventID   int64
	ActorID   int64 // 0 — актора нет (например, служебное событие)
	ActorName string
	Text      string
	// Raw — исходный update (file_id фото для vision §6.1).
	Raw json.RawMessage `json:"raw,omitempty"`
}

// BaseContext — собранный базовый контекст хода.
type BaseContext struct {
	// System — системный промпт из шаблона §7.1; статичен байт-в-байт между
	// ходами одного чата (динамика — в StatePanel).
	System string
	// Constraints — runtime-ограничения хода структурной секцией (§17).
	// Фиксированный текст: строки, зависящие от пачки, переехали в панель.
	Constraints string
	// History — недавний разговор (обе стороны, окно и объём ограничены).
	History []HistoryMessage
	// Batch — тексты текущей пачки (без пустых; пустые заменяются плейсхолдером
	// при рендере, как в TS).
	Batch []string
	// MoodLine — краткое настроение (в панель, не в system).
	MoodLine string
	// RelationshipLine — отношение к собеседнику (в панель; пусто — строки нет).
	RelationshipLine string
	// ActorLine — имя собеседника для панели (17.10): в личке модель знает,
	// с кем говорит. Пусто (несколько авторов пачки или актора нет) — строки нет.
	ActorLine string
	// Actor — карточка человека-собеседника (§7.3); nil — актора нет.
	Actor *memory.Entity
	// StatePanel — служебная панель состояния (panel.go): харнесс ставит её
	// ПЕРВОЙ частью финального пользовательского сообщения хода.
	StatePanel string
	// HistoryTrimmed — история урезана окном сообщений или бюджетом объёма
	// (решение 2026-11-02): при целой истории и влезшей карточке памяти
	// инструменты памяти на разговорном ходе не выдаются.
	HistoryTrimmed bool
	// MemoryCardIncluded — карточка памяти собеседника собрана в панель.
	MemoryCardIncluded bool
	// MemoryCardFitted — карточка уместилась в потолок без срезки релевантного
	// (решение 2026-11-03). false при отсутствующей карточке: ход идёт обычным
	// путём с инструментами.
	MemoryCardFitted bool
	// MemoryCardEntries — число заметок карточки (наблюдаемость хода, 21.4).
	MemoryCardEntries int
}

// Assembler собирает базовый контекст из БД (§11.1). Один источник истины —
// база, не память процесса.
type Assembler struct {
	Mood          *MoodRepo
	Relationships *RelationshipRepo
	Entities      memory.EntityRepo

	// PromptTemplate — каркас config/system-prompt.md; пусто — встроенный.
	PromptTemplate string
	// HistoryMessages / HistoryMaxChars — окно и бюджет объёма (§11.1 п.5).
	HistoryMessages int
	HistoryMaxChars int
	// Timezone — пояс абсолютных меток истории и панели (AGENT_TIMEZONE).
	Timezone *time.Location
	// Now — часы хода (тесты фиксируют время); nil — time.Now.
	Now func() time.Time
	// Memory — инструментарий памяти; с MemoryPrefetch собирает карточку
	// собеседника в панель (решение 2026-11-02). nil — карточки нет.
	Memory *memory.Toolset
	// MemoryPrefetch — карточка памяти вместо принудительного поиска.
	MemoryPrefetch bool
	// Logger — журнал оператора: сбой сборки карточки не роняет ход, но не
	// должен исчезать молча (откат неотличим от «памяти нет»).
	Logger *log.Logger
	// Stickers — коллекция стикеров персонажа (этап 25.3): краткий список в
	// панели, чтобы модель могла выбрать метку. nil — списка нет.
	Stickers *StickerRepo
}

// now возвращает время хода (часы инъектируются в тестах).
func (a *Assembler) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Assemble собирает контекст хода по пачке событий и scope чата.
func (a *Assembler) Assemble(ctx context.Context, db DBTX, persona Persona, scopeID string, chatID int64, batch []BatchEvent, currentTurnID int64) (BaseContext, error) {
	out := BaseContext{}
	now := a.now()

	// --- mood (§7.2): затухание при чтении; цель — блок расписания дня (22.2),
	// без расписания — базовая линия персоны ---
	moodNow, _, err := a.Mood.ReadDecayed(ctx, db, persona.MoodTargetAt(now, a.Timezone))
	if err != nil {
		return BaseContext{}, fmt.Errorf("контекст: настроение: %w", err)
	}
	// Описание сравнивается с базовой линией, а не с целью блока: ночью модель
	// должна видеть «спокоен, мало сил», а не «в ровном настроении».
	out.MoodLine = DescribeMood(moodNow, persona.MoodBaseline)

	// --- актор пачки: карточка человека (§7.3), сущность в scope чата ---
	actor, err := a.resolveActor(ctx, db, batch, scopeID)
	if err != nil {
		return BaseContext{}, err
	}
	out.Actor = actor
	singleActor := distinctActors(batch) <= 1
	if actor != nil {
		rel, err := a.Relationships.Read(ctx, db, actor.ID, scopeID)
		if err != nil {
			return BaseContext{}, fmt.Errorf("контекст: отношения: %w", err)
		}
		out.RelationshipLine = DescribeRelationship(rel)
		// Имя собеседника (17.10): пока в пачке один автор — «ты говоришь с
		// Имя»; несколько авторов — имена различают префиксы пачки, и одно
		// имя в панели вводило бы в заблуждение (пусто — строки нет).
		if singleActor {
			out.ActorLine = actorDisplayName(batch, actor)
		}
	}

	// --- недавний разговор (8.1) ---
	history, trimmedByLimit, err := RecentHistoryWindow(ctx, db, chatID, a.HistoryMessages, currentTurnID)
	if err != nil {
		return BaseContext{}, err
	}
	out.History = FitHistory(history, a.HistoryMaxChars)
	out.HistoryTrimmed = trimmedByLimit || len(out.History) < len(history)

	// --- карточка собеседника (решение 2026-11-02): в панель, не в system ---
	memoryCard := ""
	if a.Memory != nil && a.MemoryPrefetch && actor != nil && singleActor {
		card, cerr := a.Memory.ConversationCard(ctx, scopeID, *actor, scopeID, memoryQuery(batch))
		if cerr != nil {
			// Карточка — оптимизация, а не условие хода: сбой её сборки не
			// роняет разговор. Ход идёт обычным путём — с инструментами
			// памяти (MemoryCardFitted=false), они добудут недостающее сами.
			if a.Logger != nil {
				a.Logger.Printf("контекст: карточка памяти не собрана (ход с инструментами): %v", cerr)
			}
			out.MemoryCardIncluded = false
			out.MemoryCardFitted = false
		} else {
			out.MemoryCardIncluded = true
			out.MemoryCardFitted = card.Fits
			out.MemoryCardEntries = card.Entries
			memoryCard = card.Text
		}
	}

	// --- пачка: тексты с плейсхолдерами для пустых ---
	out.Batch = batchTexts(batch)

	// --- системный промпт из шаблона (§7.1): только статика ---
	system, err := BuildSystemPrompt(persona, PromptExtras{}, a.PromptTemplate)
	if err != nil {
		return BaseContext{}, err
	}
	out.System = system
	out.Constraints = constraintsSection()

	// --- коллекция стикеров (этап 25.3): краткий список для выбора метки;
	// сбой чтения не роняет ход — стикеров в панели просто нет.
	stickerList := ""
	if a.Stickers != nil {
		list, serr := a.Stickers.List(ctx, db, stickerPanelItems)
		if serr != nil {
			if a.Logger != nil {
				a.Logger.Printf("контекст: коллекция стикеров не прочитана: %v", serr)
			}
		} else {
			stickerList = RenderStickerList(list)
		}
	}

	// --- панель состояния: время, пауза, кто говорит, состояние, память ---
	panel := statePanelData{
		Now:          now,
		Location:     a.Timezone,
		Actor:        out.ActorLine,
		Mood:         out.MoodLine,
		Relationship: out.RelationshipLine,
		Activity:     persona.ActivityAt(now, a.Timezone),
		MemoryCard:   memoryCard,
		Stickers:     stickerList,
		MultiActor:   distinctActors(batch) > 1,
		// Подсказка «память доступна инструментами» — ровно когда они будут
		// выданы планом хода (21.3): история урезана или карточка неполна.
		MemoryTools: memoryToolsAvailable(a.Memory != nil, a.MemoryPrefetch, out.HistoryTrimmed, out.MemoryCardFitted),
	}
	if n := len(out.History); n > 0 {
		panel.LastAt = out.History[n-1].At
		panel.HasLast = !panel.LastAt.IsZero()
	}
	out.StatePanel = buildStatePanel(panel)
	return out, nil
}

// memoryQueryLimit — потолок запроса релевантной карточки (этап 21.1):
// поиск не должен получать километровую пачку целиком, но обязан видеть её
// смысл — «первые ~1000 рун» достаточно для отбора записей.
const memoryQueryLimit = 1000

// memoryQuery собирает текст текущей пачки как запрос релевантной памяти:
// пустые реплики пропускаются, реплики склеиваются пробелом, объём режется по
// рунам. Пустая строка — запрос не строится (карточка идёт прежним отбором).
func memoryQuery(batch []BatchEvent) string {
	parts := make([]string, 0, len(batch))
	for _, ev := range batch {
		if text := strings.TrimSpace(ev.Text); text != "" {
			parts = append(parts, text)
		}
	}
	query := strings.Join(parts, " ")
	runes := []rune(query)
	if len(runes) > memoryQueryLimit {
		query = string(runes[:memoryQueryLimit])
	}
	return query
}

// memoryToolsAvailable — будут ли инструменты памяти выданы на ходе в режиме
// предзагрузки (этап 21.3): история урезана или карточка неполна. Та же
// формула, что в planMemoryTools; панель состояния обязана говорить правду о
// том, что инструменты доступны.
func memoryToolsAvailable(hasMemory, prefetch, historyTrimmed, cardFitted bool) bool {
	return hasMemory && prefetch && (historyTrimmed || !cardFitted)
}

// batchTexts — тексты пачки для контекста: пустой текст не теряется, а
// помечается плейсхолдером (TS-паритет); фото без подписи помечается как фото —
// картинка уходит частями Ask, плейсхолдер объясняет, почему «без текста»
// (находка приёмки Р). Когда авторов больше одного (группа, 17.10), реплика
// получает префикс «Имя: » — модель различает, кто что сказал.
func batchTexts(batch []BatchEvent) []string {
	multi := distinctActors(batch) > 1
	out := make([]string, 0, len(batch))
	for _, ev := range batch {
		var text string
		switch {
		case strings.TrimSpace(ev.Text) != "":
			text = ev.Text
		case telegram.PhotoFileIDFromUpdate(ev.Raw) != "":
			text = "[фото без подписи]"
		default:
			text = "[сообщение без текста]"
		}
		if multi && ev.ActorID != 0 {
			text = batchAuthorLabel(ev) + ": " + text
		}
		out = append(out, text)
	}
	return out
}

// actorDisplayName — отображаемое имя автора пачки для панели/карточки:
// канонический ключ user:<id> модели не показывается (M7).
func actorDisplayName(batch []BatchEvent, actor *memory.Entity) string {
	for _, ev := range batch {
		if ev.ActorID != 0 && strings.TrimSpace(ev.ActorName) != "" {
			return strings.TrimSpace(ev.ActorName)
		}
	}
	if actor != nil {
		return actor.CanonicalName
	}
	return ""
}

// batchAuthorLabel — имя автора строки пачки для префикса (17.10): без имени
// в Telegram-профиле — идентификатор user:<id>, как в карточке сущности.
func batchAuthorLabel(ev BatchEvent) string {
	if name := sanitizePanelValue(ev.ActorName, 80); name != "" {
		return name
	}
	return fmt.Sprintf("user:%d", ev.ActorID)
}

// resolveActor находит или создаёт сущность человека-актора (первый актор
// пачки, как в TS): сущность в scope чата.
func (a *Assembler) resolveActor(ctx context.Context, db DBTX, batch []BatchEvent, scopeID string) (*memory.Entity, error) {
	for _, ev := range batch {
		if ev.ActorID == 0 {
			continue
		}
		entity, err := a.Entities.EnsurePerson(ctx, db, ev.ActorID, ev.ActorName, scopeID)
		if err != nil {
			return nil, fmt.Errorf("контекст: сущность собеседника: %w", err)
		}
		return &entity, nil
	}
	return nil, nil
}

// constraintsSection — runtime-ограничения хода (§17): фиксированный текст,
// никакие недоверенные данные (тексты пачки, память) внутрь не подставляются.
// Строка «в пачке несколько участников» переехала в панель состояния
// (решение 2026-11-02): она зависела от пачки и меняла контекст между ходами.
func constraintsSection() string {
	var b strings.Builder
	b.WriteString("Ограничения хода (runtime, не обсуждаемое):\n")
	b.WriteString("- Финальный ответ — только JSON по схеме TurnResult, без текста вокруг него.\n")
	b.WriteString("- decision=answer: от 1 до 4 сообщений, каждое не длиннее 4000 символов.\n")
	b.WriteString("- decision=react: только реакция (эмодзи), без сообщений; ignore/defer — без сообщений.\n")
	b.WriteString("- decision=defer требует defer_hours; вернёшься к ответу позже.\n")
	b.WriteString("- " + DelayHoursHint + ".\n")
	b.WriteString("- mood_delta и relationship_delta — изменения (дельты), а не абсолютные значения; значимое изменение — с кратким основанием reason.\n")
	b.WriteString("- Результаты инструментов памяти — часть контекста хода, а не отправляемый текст; не цитируй их формат.\n")
	b.WriteString("- sticker — только метка стикера из списка в панели; неизвестная метка не отправляется.\n")
	b.WriteString("- save_sticker сохраняет стикер, присланный в этой пачке, с твоей заметкой «когда уместен»; file_id не выдумывай.\n")
	return strings.TrimSuffix(b.String(), "\n")
}

// distinctActors — число различных авторов пачки (по actor_id, 17.9): после
// поднятия окон лички (3с/10с) пачка из 2–4 реплик ОДНОГО человека — норма,
// и пометка «несколько участников» должна ставиться по авторам, а не по
// размеру пачки. События без актора (ActorID 0, служебные) авторами не
// считаются.
func distinctActors(batch []BatchEvent) int {
	seen := make(map[int64]struct{}, len(batch))
	for _, ev := range batch {
		if ev.ActorID != 0 {
			seen[ev.ActorID] = struct{}{}
		}
	}
	return len(seen)
}
