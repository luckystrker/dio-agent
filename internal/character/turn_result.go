// TurnResult (§11.2) — контракт результата хода. Ответ модели обязан
// валидироваться по схеме до применения; невалидный ответ не применяется —
// retry по политике (один ограниченный repair §11.3), затем явная ошибка хода,
// а не «лучшее усилие». Значения mood в stateChanges — дельты, не абсолютное
// состояние: абсолютная перезапись моделью запрещена. unknown[] обзорного
// интерфейса спеки здесь закрытые типы с ограничениями размеров.
//
// Правила результата (§11.2): ignore/defer без сообщений; react с допустимой
// реакцией; speak/answer 1–4 сообщения; defer требует reply_later с notBefore
// (поле defer_hours — относительное время, намерение создаёт runtime).
//
// Пределы строк перенесены из TS (turn-result.ts, чек-лист 1.10): сообщение
// ≤ 4000 (граница Telegram), заметка ≤ 2000.
package character

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Лимиты контракта результата (общие для валидатора и JSON-схемы).
const (
	maxMessages      = 4
	maxMessageLength = 4000
	maxNotes         = 5
	maxNoteLength    = 2000
	maxMemories      = 5
	maxMemoryText    = 2000
	maxEntityName    = 200
	maxEntityLinks   = 3
	maxIntentions    = 2
	maxReasonLength  = 500
	maxReactionLen   = 64
	minDeferHours    = 0.05
	maxDeferHours    = 168
	minDelayHours    = 0.05
)

// Действия хода (§11.2). В JSON-схеме — decision.
const (
	DecisionAnswer = "answer"
	DecisionReact  = "react"
	DecisionIgnore = "ignore"
	DecisionDefer  = "defer"
)

// MemoryCandidate — кандидат долговременной памяти (§10.1): модель предлагает,
// runtime валидирует и назначает источники (§10.1 п.3: модель ссылок не
// присваивает вовсе).
type MemoryCandidate struct {
	Kind       string       `json:"kind"` // claim | experience | thought | preference
	Text       string       `json:"text"`
	Entities   []EntityLink `json:"entities,omitempty"`
	Salience   *float64     `json:"salience,omitempty"`
	Confidence *float64     `json:"confidence,omitempty"` // допустим только у claim
}

// EntityLink — сущность кандидата памяти.
type EntityLink struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"` // person | topic | place | object | event | project
	Role string `json:"role,omitempty"` // subject | participant | place | context
}

// IntentionProposal — предложение отложенного действия (§10.1 ≤2/ход, §14.1):
// время относительное, в часах.
type IntentionProposal struct {
	Kind       string  `json:"kind"` // reply_later | check_in | share | explore
	Topic      string  `json:"topic"`
	Reason     string  `json:"reason"`
	DelayHours float64 `json:"delay_hours"`
}

// MoodDelta — изменение настроения за ход (§7.2): дельты, с основанием.
type MoodDelta struct {
	Valence      *float64 `json:"valence,omitempty"`
	Arousal      *float64 `json:"arousal,omitempty"`
	SocialEnergy *float64 `json:"socialEnergy,omitempty"`
	Reason       string   `json:"reason"`
}

// RelationshipDelta — изменение отношений (§7.3): ограниченно, с основанием;
// familiarity моделью не задаётся.
type RelationshipDelta struct {
	Affinity *float64 `json:"affinity,omitempty"`
	Trust    *float64 `json:"trust,omitempty"`
	Reason   string   `json:"reason"`
}

// TurnResult — строгий контракт результата хода (§11.2).
type TurnResult struct {
	Decision          string              `json:"decision"`
	Messages          []string            `json:"messages,omitempty"`
	Notes             []string            `json:"notes,omitempty"`
	Memory            []MemoryCandidate   `json:"memory,omitempty"`
	Reaction          string              `json:"reaction,omitempty"`
	Intentions        []IntentionProposal `json:"intentions,omitempty"`
	DeferHours        *float64            `json:"defer_hours,omitempty"`
	MoodDelta         *MoodDelta          `json:"mood_delta,omitempty"`
	RelationshipDelta *RelationshipDelta  `json:"relationship_delta,omitempty"`
	Reason            string              `json:"reason,omitempty"`
}

// ParseTurnResult разбирает и валидирует ответ модели: строгий JSON (неизвестные
// поля запрещены — additionalProperties: false схемы), без «мусора» после
// объекта + правила §11.2.
func ParseTurnResult(raw []byte) (TurnResult, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r TurnResult
	if err := dec.Decode(&r); err != nil {
		return TurnResult{}, fmt.Errorf("turnresult: %w", err)
	}
	if dec.More() {
		return TurnResult{}, fmt.Errorf("turnresult: после JSON-объекта есть лишний текст")
	}
	if err := r.Validate(); err != nil {
		return TurnResult{}, err
	}
	return r, nil
}

func rangeErr(what string, v, min, max float64) error {
	return fmt.Errorf("turnresult: %s должно быть числом от %v до %v, получено %v", what, min, max, v)
}

// runeLen — длина строки в символах (рунах), как .length в TS; Telegram-лимит
// тоже в символах, а не в байтах.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

func optInRange(what string, v *float64, min, max float64) error {
	if v == nil {
		return nil
	}
	if *v < min || *v > max || !(*v == *v) { // NaN отсекается сравнением с собой
		return rangeErr(what, *v, min, max)
	}
	return nil
}

func validReason(reason, what string) error {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" || runeLen(reason) > maxReasonLength {
		return fmt.Errorf("turnresult: %s требует непустое основание reason до %d символов", what, maxReasonLength)
	}
	return nil
}

// Validate проверяет структуру и правила результата (§11.2).
func (r TurnResult) Validate() error {
	switch r.Decision {
	case DecisionAnswer, DecisionReact, DecisionIgnore, DecisionDefer:
	default:
		return fmt.Errorf("turnresult: неизвестное decision %q — допустимы answer|react|ignore|defer", r.Decision)
	}

	for _, m := range r.Messages {
		if runeLen(m) == 0 || runeLen(m) > maxMessageLength {
			return fmt.Errorf("turnresult: сообщение должно быть от 1 до %d символов, получено %d", maxMessageLength, runeLen(m))
		}
	}
	if len(r.Messages) > maxMessages {
		return fmt.Errorf("turnresult: не более %d сообщений, получено %d", maxMessages, len(r.Messages))
	}
	for _, n := range r.Notes {
		if runeLen(n) == 0 || runeLen(n) > maxNoteLength {
			return fmt.Errorf("turnresult: заметка должна быть от 1 до %d символов, получено %d", maxNoteLength, runeLen(n))
		}
	}
	if len(r.Notes) > maxNotes {
		return fmt.Errorf("turnresult: не более %d заметок, получено %d", maxNotes, len(r.Notes))
	}
	if len(r.Memory) > maxMemories {
		return fmt.Errorf("turnresult: не более %d кандидатов памяти, получено %d", maxMemories, len(r.Memory))
	}
	for i, c := range r.Memory {
		if err := c.validate(i); err != nil {
			return err
		}
	}
	if r.Reaction != "" && runeLen(r.Reaction) > maxReactionLen {
		return fmt.Errorf("turnresult: reaction до %d символов", maxReactionLen)
	}
	if len(r.Intentions) > maxIntentions {
		return fmt.Errorf("turnresult: не более %d предложений намерений, получено %d", maxIntentions, len(r.Intentions))
	}
	for i, p := range r.Intentions {
		switch p.Kind {
		case "reply_later", "check_in", "share", "explore":
		default:
			return fmt.Errorf("turnresult: намерение %d: неизвестный kind %q", i, p.Kind)
		}
		if strings.TrimSpace(p.Topic) == "" || runeLen(p.Topic) > 200 {
			return fmt.Errorf("turnresult: намерение %d: topic от 1 до 200 символов", i)
		}
		if strings.TrimSpace(p.Reason) == "" || runeLen(p.Reason) > maxReasonLength {
			return fmt.Errorf("turnresult: намерение %d: reason от 1 до %d символов", i, maxReasonLength)
		}
		if p.DelayHours < minDelayHours || p.DelayHours > maxDeferHours {
			return rangeErr(fmt.Sprintf("намерение %d: delay_hours", i), p.DelayHours, minDelayHours, maxDeferHours)
		}
	}
	if err := optInRange("defer_hours", r.DeferHours, minDeferHours, maxDeferHours); err != nil {
		return err
	}
	if r.MoodDelta != nil {
		if err := validReason(r.MoodDelta.Reason, "mood_delta"); err != nil {
			return err
		}
		if err := optInRange("mood_delta.valence", r.MoodDelta.Valence, -1, 1); err != nil {
			return err
		}
		if err := optInRange("mood_delta.arousal", r.MoodDelta.Arousal, 0, 1); err != nil {
			return err
		}
		if err := optInRange("mood_delta.socialEnergy", r.MoodDelta.SocialEnergy, 0, 1); err != nil {
			return err
		}
	}
	if r.RelationshipDelta != nil {
		if err := validReason(r.RelationshipDelta.Reason, "relationship_delta"); err != nil {
			return err
		}
		if err := optInRange("relationship_delta.affinity", r.RelationshipDelta.Affinity, -1, 1); err != nil {
			return err
		}
		if err := optInRange("relationship_delta.trust", r.RelationshipDelta.Trust, 0, 1); err != nil {
			return err
		}
	}
	if runeLen(r.Reason) > maxReasonLength {
		return fmt.Errorf("turnresult: reason до %d символов", maxReasonLength)
	}

	// --- правила действия (§11.2) ---
	switch r.Decision {
	case DecisionAnswer:
		if len(r.Messages) == 0 {
			return fmt.Errorf("turnresult: answer требует от 1 до %d сообщений", maxMessages)
		}
	case DecisionReact:
		if r.Reaction == "" {
			return fmt.Errorf("turnresult: react требует reaction (эмодзи)")
		}
		if len(r.Messages) > 0 {
			return fmt.Errorf("turnresult: react не содержит сообщений")
		}
	default: // ignore / defer
		if len(r.Messages) > 0 {
			return fmt.Errorf("turnresult: ignore/defer не содержат отправляемых сообщений")
		}
	}
	if r.Decision != DecisionReact && r.Reaction != "" {
		return fmt.Errorf("turnresult: reaction допустим только при decision=react")
	}
	if r.Decision == DecisionDefer && r.DeferHours == nil {
		return fmt.Errorf("turnresult: defer требует defer_hours (reply_later с notBefore, §11.2)")
	}
	if r.Decision != DecisionDefer && r.DeferHours != nil {
		return fmt.Errorf("turnresult: defer_hours допустим только при decision=defer")
	}
	return nil
}

var (
	memoryKinds     = map[string]bool{"claim": true, "experience": true, "thought": true, "preference": true}
	entityKinds     = map[string]bool{"person": true, "topic": true, "place": true, "object": true, "event": true, "project": true}
	entityRoles     = map[string]bool{"subject": true, "participant": true, "place": true, "context": true}
	defaultSalience = 0.5
)

func (c MemoryCandidate) validate(i int) error {
	if !memoryKinds[c.Kind] {
		return fmt.Errorf("turnresult: память %d: неизвестный kind %q", i, c.Kind)
	}
	if strings.TrimSpace(c.Text) == "" || runeLen(c.Text) > maxMemoryText {
		return fmt.Errorf("turnresult: память %d: text от 1 до %d символов", i, maxMemoryText)
	}
	if len(c.Entities) > maxEntityLinks {
		return fmt.Errorf("turnresult: память %d: не более %d сущностей", i, maxEntityLinks)
	}
	for j, e := range c.Entities {
		if strings.TrimSpace(e.Name) == "" || runeLen(e.Name) > maxEntityName {
			return fmt.Errorf("turnresult: память %d, сущность %d: name от 1 до %d символов", i, j, maxEntityName)
		}
		if e.Kind != "" && !entityKinds[e.Kind] {
			return fmt.Errorf("turnresult: память %d, сущность %d: неизвестный kind %q", i, j, e.Kind)
		}
		if e.Role != "" && !entityRoles[e.Role] {
			return fmt.Errorf("turnresult: память %d, сущность %d: неизвестная роль %q", i, j, e.Role)
		}
	}
	if c.Salience != nil && (*c.Salience < 0 || *c.Salience > 1) {
		return rangeErr(fmt.Sprintf("память %d: salience", i), *c.Salience, 0, 1)
	}
	if c.Confidence != nil {
		if c.Kind != "claim" {
			return fmt.Errorf("turnresult: память %d: confidence допустим только у claim", i)
		}
		if *c.Confidence < 0 || *c.Confidence > 1 {
			return rangeErr(fmt.Sprintf("память %d: confidence", i), *c.Confidence, 0, 1)
		}
	}
	return nil
}

// SalienceOrDefault — salience кандидата; не задан — 0.5 (паритет default TS).
func (c MemoryCandidate) SalienceOrDefault() float64 {
	if c.Salience == nil {
		return defaultSalience
	}
	return *c.Salience
}
