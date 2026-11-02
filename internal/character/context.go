// Сборка базового контекста хода (§11.1): то, что приложение собирает всегда,
// независимо от поведения модели. Пункты §11.1:
//  1. core persona + runtime constraints (constraints — отдельная структурная
//     секция контекста, не склеенная с недоверенными данными, §17);
//  2. текущий batch (цель фоновой работы приходит в 9.6);
//  3. краткое текущее mood;
//  4. разрешённые отношения с участниками;
//  5. последние сообщения чата, обе стороны (окно и потолок объёма из
//     конфигурации), только доставленные ответы.
//
// Тематические карточки памяти в базовый контекст v0.7 не кладутся (§11.1):
// модель добирает их сама инструментами §10.4 — метка {{memory}} остаётся
// пустой, и её абзац исчезает из промпта.
package character

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	// System — системный промпт из шаблона §7.1.
	System string
	// Constraints — runtime-ограничения хода структурной секцией (§17).
	Constraints string
	// History — недавний разговор (обе стороны, окно и объём ограничены).
	History []HistoryMessage
	// Batch — тексты текущей пачки (без пустых; пустые заменяются плейсхолдером
	// при рендере, как в TS).
	Batch []string
	// MoodLine — краткое настроение для метки {{mood}}.
	MoodLine string
	// RelationshipLine — отношение к собеседнику (пусто — абзац шаблона исчезает).
	RelationshipLine string
	// ActorLine — имя собеседника для метки {{actor}} (17.10): в личке модель
	// знает, с кем говорит. Пусто (несколько авторов пачки или актора нет) —
	// абзац шаблона исчезает.
	ActorLine string
	// Actor — карточка человека-собеседника (§7.3); nil — актора нет.
	Actor *memory.Entity
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
}

// Assemble собирает контекст хода по пачке событий и scope чата.
func (a *Assembler) Assemble(ctx context.Context, db DBTX, persona Persona, scopeID string, chatID int64, batch []BatchEvent, currentTurnID int64) (BaseContext, error) {
	out := BaseContext{}

	// --- mood (§7.2): затухание при чтении ---
	moodNow, _, err := a.Mood.ReadDecayed(ctx, db, persona.MoodBaseline)
	if err != nil {
		return BaseContext{}, fmt.Errorf("контекст: настроение: %w", err)
	}
	out.MoodLine = DescribeMood(moodNow, persona.MoodBaseline)

	// --- актор пачки: карточка человека (§7.3), сущность в scope чата ---
	actor, err := a.resolveActor(ctx, db, batch, scopeID)
	if err != nil {
		return BaseContext{}, err
	}
	out.Actor = actor
	if actor != nil {
		rel, err := a.Relationships.Read(ctx, db, actor.ID, scopeID)
		if err != nil {
			return BaseContext{}, fmt.Errorf("контекст: отношения: %w", err)
		}
		out.RelationshipLine = DescribeRelationship(rel)
		// Имя собеседника (17.10): пока в пачке один автор — «Ты говоришь с
		// Имя»; несколько авторов — имена различают префиксы пачки, и одно
		// имя в system вводило бы в заблуждение (пусто — абзац исчезает).
		if distinctActors(batch) <= 1 {
			out.ActorLine = actor.CanonicalName
		}
	}

	// --- недавний разговор (8.1) ---
	history, err := RecentHistory(ctx, db, chatID, a.HistoryMessages, currentTurnID)
	if err != nil {
		return BaseContext{}, err
	}
	out.History = FitHistory(history, a.HistoryMaxChars)

	// --- пачка: тексты с плейсхолдерами для пустых ---
	out.Batch = batchTexts(batch)

	// --- системный промпт из шаблона (§7.1) ---
	system, err := BuildSystemPrompt(persona, PromptExtras{
		MoodLine:         out.MoodLine,
		RelationshipLine: out.RelationshipLine,
		ActorLine:        out.ActorLine,
	}, a.PromptTemplate)
	if err != nil {
		return BaseContext{}, err
	}
	out.System = system
	out.Constraints = constraintsSection(batch)
	return out, nil
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

// batchAuthorLabel — имя автора строки пачки для префикса (17.10): без имени
// в Telegram-профиле — идентификатор user:<id>, как в карточке сущности.
func batchAuthorLabel(ev BatchEvent) string {
	if ev.ActorName != "" {
		return ev.ActorName
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
		name := ev.ActorName
		if name == "" {
			name = fmt.Sprintf("user:%d", ev.ActorID)
		}
		entity, err := a.Entities.FindOrCreate(ctx, db, "person", name, scopeID)
		if err != nil {
			return nil, fmt.Errorf("контекст: сущность собеседника: %w", err)
		}
		return &entity, nil
	}
	return nil, nil
}

// constraintsSection — runtime-ограничения хода (§17): фиксированный текст,
// никакие недоверенные данные (тексты пачки, память) внутрь не подставляются.
func constraintsSection(batch []BatchEvent) string {
	var b strings.Builder
	b.WriteString("Ограничения хода (runtime, не обсуждаемое):\n")
	b.WriteString("- Финальный ответ — только JSON по схеме TurnResult, без текста вокруг него.\n")
	b.WriteString("- decision=answer: от 1 до 4 сообщений, каждое не длиннее 4000 символов.\n")
	b.WriteString("- decision=react: только реакция (эмодзи), без сообщений; ignore/defer — без сообщений.\n")
	b.WriteString("- decision=defer требует defer_hours; вернёшься к ответу позже.\n")
	b.WriteString("- mood_delta и relationship_delta — изменения (дельты), а не абсолютные значения; значимое изменение — с кратким основанием reason.\n")
	b.WriteString("- Результаты инструментов памяти — часть контекста хода, а не отправляемый текст; не цитируй их формат.\n")
	if distinctActors(batch) > 1 {
		b.WriteString("- В пачке несколько участников: отвечай на пачку целиком, а не только последнему.\n")
	}
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
