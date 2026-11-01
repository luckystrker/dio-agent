// Применение хода (§12.1): валидированный результат превращается в изменения
// состояния одной транзакцией PostgreSQL — дельты mood/отношений, записи
// памяти, намерения, строки outbox и события deltas_applied + delivery_queued
// (§12.1: «либо всё, либо ничего»). Однократность применения выводится из
// однократности записи журнала: CAS строки turns + SetHistoryTx с проверкой
// префикса — повторный прогон ничего не меняет (инвариант 4).
package character

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luckystrker/dio-agent/internal/memory"
)

// isNoRows отличает «строки нет» от прочих ошибок запроса.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// intentionTTLHours — время жизни намерения после notBefore (§14.1: любое
// исполнимое намерение имеет expiresAt; паритет INTENTION_TTL_HOURS TS).
const intentionTTLHours = 72

// ErrApplyAborted — хук Extra транзакции применения отменил применение
// (например CAS намерения проиграл гонку: исполнение уже сделано другим).
// Применение отвечает Applied=false без изменений (транзакция откатывается).
var ErrApplyAborted = errors.New("применение отменено хуком транзакции")

// ApplyResult — что именно применилось (для события deltas_applied и лога §20).
type ApplyResult struct {
	Applied         bool // false — ход уже был применён (повтор отклонён)
	MemoryIDs       []int64
	IntentionIDs    []int64
	OutboxIDs       []int64
	MoodNext        *Mood
	SkippedReaction bool // react без целевого сообщения — реакция пропущена
	// DuplicateIntentions — предложения модели, отклонённые живым дублем
	// (§14.1): причина дубля (какое намерение живо) видна оператору в логе.
	DuplicateIntentions []DuplicateIntention
}

// DuplicateIntention — отклонённое живым дублем предложение намерения.
type DuplicateIntention struct {
	Kind       string
	Topic      string
	LiveStatus string // статус живого намерения с тем же dedupe-ключом
}

// ApplyOptions — вход транзакции применения.
type ApplyOptions struct {
	TurnID  int64
	ChatID  int64
	ScopeID string
	Result  TurnResult
	// PersonaBaseline — базовая линия настроения для затухания (§7.2).
	PersonaBaseline Mood
	// Journal — пишет deltas_applied + delivery_queued в этой же транзакции.
	Journal *TurnJournal
	// Extra — дополнительная работа ВНУТРИ транзакции применения: вызывается
	// после CAS хода и до записи журнала (§12.1: «либо всё, либо ничего»
	// распространяется и на связанное состояние, например CAS намерения §14.3 —
	// нужно фону 9.6, чтобы статус намерения и его доставка менялись атомарно).
	// Ошибка, оборачивающая ErrApplyAborted, откатывает транзакцию и возвращает
	// Applied=false (применение уже сделано другим победителем).
	Extra func(ctx context.Context, tx pgx.Tx) error
}

// ApplyTurn применяет валидированный результат в одной транзакции.
func ApplyTurn(ctx context.Context, pool *pgxpool.Pool, opts ApplyOptions) (ApplyResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("применение: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// --- CAS строки хода: повторное применение отклоняется (§12.1, инвариант 4) ---
	tag, err := tx.Exec(ctx,
		`UPDATE turns SET status = 'applied', applied_at = now()
		 WHERE id = $1 AND status <> 'applied'`, opts.TurnID)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("применение: CAS хода не удался: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Уже применён (другой процесс или повторный прогон): честный no-op.
		return ApplyResult{Applied: false}, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE social_events SET status = 'processed' WHERE turn_id = $1`, opts.TurnID); err != nil {
		return ApplyResult{}, fmt.Errorf("применение: события не закрыты: %w", err)
	}

	// --- хук транзакции: связанная работа тем же «всё или ничего» ---
	if opts.Extra != nil {
		if err := opts.Extra(ctx, tx); err != nil {
			if errors.Is(err, ErrApplyAborted) {
				// Гонку выиграл другой: всё откатывается, включая CAS хода выше.
				return ApplyResult{Applied: false}, nil
			}
			return ApplyResult{}, fmt.Errorf("применение: хук транзакции: %w", err)
		}
	}

	res := ApplyResult{Applied: true}

	// --- доставка (§11.2, §13.1): сообщения и/или реакция ---
	delivery := &DeliveryQueued{TurnID: opts.TurnID, ChatID: opts.ChatID}
	switch opts.Result.Decision {
	case DecisionAnswer:
		for i, text := range opts.Result.Messages {
			id, err := enqueueOutbox(ctx, tx, opts.TurnID, opts.ChatID, i, map[string]any{
				"kind": "message",
				"text": text,
			})
			if err != nil {
				return ApplyResult{}, err
			}
			res.OutboxIDs = append(res.OutboxIDs, id)
			delivery.Outbox = append(delivery.Outbox, QueuedDelivery{ID: id, Position: i, Kind: "message", Text: text})
		}
		// Необязательная reaction при answer (§11.2): цель недоступна —
		// реакция пропускается без дополнительного вызова модели.
		if opts.Result.Reaction != "" {
			target, err := reactionTarget(ctx, tx, opts.TurnID)
			if err != nil {
				return ApplyResult{}, err
			}
			if target == 0 {
				res.SkippedReaction = true
			} else {
				id, err := enqueueOutbox(ctx, tx, opts.TurnID, opts.ChatID, len(opts.Result.Messages), map[string]any{
					"kind":              "reaction",
					"reaction":          opts.Result.Reaction,
					"target_message_id": target,
				})
				if err != nil {
					return ApplyResult{}, err
				}
				res.OutboxIDs = append(res.OutboxIDs, id)
				delivery.Outbox = append(delivery.Outbox, QueuedDelivery{ID: id, Position: len(opts.Result.Messages), Kind: "reaction", Reaction: opts.Result.Reaction})
			}
		}
	case DecisionReact:
		target, err := reactionTarget(ctx, tx, opts.TurnID)
		if err != nil {
			return ApplyResult{}, err
		}
		if target == 0 {
			// Нужной реакции нет — runtime пропускает её, не создаёт вызов ради
			// emoji (§11.2); валидные дельты при этом применяются.
			res.SkippedReaction = true
		} else {
			id, err := enqueueOutbox(ctx, tx, opts.TurnID, opts.ChatID, 0, map[string]any{
				"kind":              "reaction",
				"reaction":          opts.Result.Reaction,
				"target_message_id": target,
			})
			if err != nil {
				return ApplyResult{}, err
			}
			res.OutboxIDs = append(res.OutboxIDs, id)
			delivery.Outbox = append(delivery.Outbox, QueuedDelivery{ID: id, Position: 0, Kind: "reaction", Reaction: opts.Result.Reaction})
		}
	}

	// --- память (§10.1): модель предлагает, runtime назначает источники и разрешение ---
	deltas := &DeltasApplied{}
	for i, candidate := range opts.Result.Memory {
		id, err := insertMemoryCandidate(ctx, tx, opts, candidate, i)
		if err != nil {
			return ApplyResult{}, err
		}
		res.MemoryIDs = append(res.MemoryIDs, id)
		deltas.MemoryIDs = append(deltas.MemoryIDs, id)
	}

	// --- намерения (§14.1): предложения модели + defer → reply_later (§11.2) ---
	for _, proposal := range opts.Result.Intentions {
		id, created, dup, err := createIntention(ctx, tx, intentionRow{
			kind:          proposal.Kind,
			chatID:        opts.ChatID,
			scopeID:       opts.ScopeID,
			topic:         proposal.Topic,
			reason:        proposal.Reason,
			delayHours:    proposal.DelayHours,
			relatedTurnID: opts.TurnID,
		})
		if err != nil {
			return ApplyResult{}, err
		}
		if created {
			res.IntentionIDs = append(res.IntentionIDs, id)
			deltas.IntentionIDs = append(deltas.IntentionIDs, id)
		} else if dup != nil {
			res.DuplicateIntentions = append(res.DuplicateIntentions, *dup)
		}
	}
	if opts.Result.Decision == DecisionDefer {
		topic, err := deferTopic(ctx, tx, opts.TurnID)
		if err != nil {
			return ApplyResult{}, err
		}
		id, created, dup, err := createIntention(ctx, tx, intentionRow{
			kind:          "reply_later",
			chatID:        opts.ChatID,
			scopeID:       opts.ScopeID,
			topic:         topic,
			reason:        "decision=defer: персонаж отложил ответ (§11.2)",
			delayHours:    *opts.Result.DeferHours,
			relatedTurnID: opts.TurnID,
		})
		if err != nil {
			return ApplyResult{}, err
		}
		if created {
			res.IntentionIDs = append(res.IntentionIDs, id)
			deltas.IntentionIDs = append(deltas.IntentionIDs, id)
		} else if dup != nil {
			res.DuplicateIntentions = append(res.DuplicateIntentions, *dup)
		}
	}

	// --- настроение (§7.2): сжатие дельты до лимита, затем применение с затуханием ---
	moodRepo := NewMoodRepo()
	if opts.Result.MoodDelta != nil {
		clamped := ClampMoodDelta(*opts.Result.MoodDelta)
		next, err := moodRepo.ApplyDelta(ctx, tx, opts.PersonaBaseline, clamped)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("применение: дельта настроения: %w", err)
		}
		res.MoodNext = &next
		applied := appliedMoodDelta(clamped)
		deltas.Mood = &applied
	}

	// --- отношения (§7.3): familiarity по runtime-правилу + ограниченные дельты.
	// Актор резолвится здесь же (в транзакции): сущность человека-актора
	// первого события пачки — и при генерации, и при восстановлении путь один.
	actor, err := resolveActorEntity(ctx, tx, opts.TurnID, opts.ScopeID)
	if err != nil {
		return ApplyResult{}, err
	}
	if actor != nil {
		relRepo := NewRelationshipRepo()
		if _, err := relRepo.GrowFamiliarity(ctx, tx, actor.ID, opts.ScopeID,
			RelationshipLimits.FamiliarityPerSignificantTurn); err != nil {
			return ApplyResult{}, err
		}
		if opts.Result.RelationshipDelta != nil {
			if _, err := relRepo.ApplyDelta(ctx, tx, actor.ID, opts.ScopeID,
				opts.Result.RelationshipDelta.Affinity, opts.Result.RelationshipDelta.Trust); err != nil {
				return ApplyResult{}, fmt.Errorf("применение: дельта отношений: %w", err)
			}
			// Аудит-след пишет СЖАТУЮ дельту — как применено (§7.3): реальное
			// применение сжимает внутри ApplyDelta до ±лимита хода, журнал —
			// заменитель state_notes и не имеет права заявлять больше.
			deltas.Relationships = append(deltas.Relationships, AppliedRelationshipDelta{
				PersonEntityID: actor.ID,
				ScopeID:        opts.ScopeID,
				Affinity:       clampLimitPtr(opts.Result.RelationshipDelta.Affinity, RelationshipLimits.AffinityPerTurn),
				Trust:          clampLimitPtr(opts.Result.RelationshipDelta.Trust, RelationshipLimits.TrustPerTurn),
				Reason:         opts.Result.RelationshipDelta.Reason,
			})
		}
	}
	if opts.Result.Reason != "" {
		deltas.Reason = opts.Result.Reason
	}

	// --- события deltas_applied + delivery_queued в ЭТОЙ ЖЕ транзакции (§12.1).
	// Уход в конец потока: уже применённый ход (события есть) даст
	// ErrHistoryConflict и откат — повторное применение невозможно дважды.
	if err := opts.Journal.WriteApplyTx(ctx, tx, opts.TurnID, deltas, delivery); err != nil {
		return ApplyResult{}, fmt.Errorf("применение: события хода не записаны: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("применение: транзакция не зафиксирована: %w", err)
	}
	return res, nil
}

// enqueueOutbox создаёт строку доставки; UNIQUE(turn_id, position) — внутренний
// ключ против дублей намерений отправки (§13.1).
func enqueueOutbox(ctx context.Context, tx pgx.Tx, turnID, chatID int64, position int, payload map[string]any) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("применение: payload доставки не сериализуется: %w", err)
	}
	var id int64
	err = tx.QueryRow(ctx,
		`INSERT INTO delivery_outbox (turn_id, position, chat_id, payload)
		 VALUES ($1, $2, $3, $4::jsonb)
		 ON CONFLICT (turn_id, position) DO NOTHING
		 RETURNING id`, turnID, position, chatID, string(raw)).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !isNoRows(err) {
		return 0, fmt.Errorf("применение: строка outbox не создана: %w", err)
	}
	// Конфликт: строка уже существует (повторный прогон) — берём её id.
	if err := tx.QueryRow(ctx,
		`SELECT id FROM delivery_outbox WHERE turn_id = $1 AND position = $2`,
		turnID, position).Scan(&id); err != nil {
		return 0, fmt.Errorf("применение: строка outbox не перечитана: %w", err)
	}
	return id, nil
}

// reactionTarget находит Telegram message_id первого события хода (цель реакции).
func reactionTarget(ctx context.Context, tx pgx.Tx, turnID int64) (int64, error) {
	var target int64
	err := tx.QueryRow(ctx, `
		SELECT (raw_update->'message'->>'message_id')::bigint
		FROM social_events
		WHERE turn_id = $1 AND raw_update->'message'->>'message_id' IS NOT NULL
		ORDER BY id LIMIT 1`, turnID).Scan(&target)
	if isNoRows(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("применение: цель реакции не найдена: %w", err)
	}
	return target, nil
}

// insertMemoryCandidate валидирует и вставляет кандидата памяти: источник и
// разрешение (provenance) назначает runtime (§10.1, §9.1), скоуп — скоуп хода.
func insertMemoryCandidate(ctx context.Context, tx pgx.Tx, opts ApplyOptions, candidate MemoryCandidate, index int) (int64, error) {
	source := memory.SourceRef{Kind: memory.SourceJournalEvent, Ref: fmt.Sprintf("turn:%d", opts.TurnID)}
	if _, err := (memory.SourceRepo{}).EnsureSource(ctx, tx, source); err != nil {
		return 0, fmt.Errorf("применение: источник кандидата %d: %w", index, err)
	}
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(ctx, tx,
		memory.SourceJournalEvent, source.Ref, []string{opts.ScopeID})
	if err != nil {
		return 0, fmt.Errorf("применение: разрешение кандидата %d: %w", index, err)
	}

	salience := candidate.SalienceOrDefault()
	item := memory.NewItem{
		Kind:         candidate.Kind,
		Text:         candidate.Text,
		Salience:     salience,
		Confidence:   candidate.Confidence,
		ProvenanceID: provenanceID,
		Source:       &source,
	}
	for _, link := range candidate.Entities {
		kind := link.Kind
		if kind == "" {
			kind = "topic" // паритет default TS
		}
		role := link.Role
		if role == "" {
			role = "subject"
		}
		item.Links = append(item.Links, memory.EntityLink{Name: link.Name, Kind: kind, Role: role})
	}
	inserted, err := (memory.Items{}).Insert(ctx, tx, item, opts.ScopeID)
	if err != nil {
		return 0, fmt.Errorf("применение: кандидат памяти %d отклонён: %w", index, err)
	}
	return inserted.ID, nil
}

// intentionRow — параметры создания намерения.
type intentionRow struct {
	kind          string
	chatID        int64
	scopeID       string
	topic         string
	reason        string
	delayHours    float64
	relatedTurnID int64
}

// IntentionParams — экспортированные параметры создания намерения (§14.1):
// нужны фону (9.6), который тоже создаёт намерения (share по находке §15).
type IntentionParams struct {
	Kind          string // reply_later | check_in | share | explore
	ChatID        int64  // целевой чат
	ScopeID       string
	Topic         string
	Reason        string
	DelayHours    float64 // notBefore = now + delayHours; expiresAt = notBefore + 72ч
	RelatedTurnID int64
}

// CreateIntentionTx создаёт намерение внутри чужой транзакции с dedupe по
// (kind, topic, chat) — перефразировки не размножают действие (§14.1).
// Дубль считается только по живым намерениям (pending/running, блок 17.5).
// Разрешение (provenance) назначает runtime по источнику хода (§9.1).
// created=false — живой дубль: намерение с таким dedupe-ключом уже есть.
func CreateIntentionTx(ctx context.Context, tx pgx.Tx, p IntentionParams) (id int64, created bool, err error) {
	id, created, _, err = createIntention(ctx, tx, intentionRow{
		kind:          p.Kind,
		chatID:        p.ChatID,
		scopeID:       p.ScopeID,
		topic:         p.Topic,
		reason:        p.Reason,
		delayHours:    p.DelayHours,
		relatedTurnID: p.RelatedTurnID,
	})
	return id, created, err
}

// createIntention вставляет намерение с dedupe по (kind, topic, chat) —
// перефразировки не размножают действие (§14.1). Уникальность dedupe_key —
// частичная, только на живые намерения (pending/running, блок 17.5): конфликтом
// считается дубль именно живого намерения, предикат повторяет индекс
// intentions_dedupe_live_uidx. Разрешение (provenance) назначает runtime по
// источнику хода (§9.1). created=false — живой дубль, причина — в Duplicate.
func createIntention(ctx context.Context, tx pgx.Tx, row intentionRow) (int64, bool, *DuplicateIntention, error) {
	ref := fmt.Sprintf("turn:%d", row.relatedTurnID)
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(ctx, tx,
		memory.SourceJournalEvent, ref, []string{row.scopeID})
	if err != nil {
		return 0, false, nil, fmt.Errorf("применение: разрешение намерения: %w", err)
	}
	notBefore := time.Now().Add(time.Duration(row.delayHours * float64(time.Hour)))
	expiresAt := notBefore.Add(intentionTTLHours * time.Hour)
	dedupe := intentionDedupeKey(row.kind, row.topic, row.chatID)

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO intentions (kind, target_chat_id, topic, reason, provenance_id, not_before, expires_at, dedupe_key, related_turn_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (dedupe_key) WHERE status IN ('pending', 'running') DO NOTHING
		RETURNING id`,
		row.kind, row.chatID, strings.TrimSpace(row.topic), row.reason, provenanceID,
		notBefore, expiresAt, dedupe, row.relatedTurnID,
	).Scan(&id)
	if err == nil {
		return id, true, nil, nil
	}
	if isNoRows(err) {
		// Живой дубль: намерение уже есть — причина дубля видна оператору.
		dup := &DuplicateIntention{Kind: row.kind, Topic: strings.TrimSpace(row.topic)}
		if serr := tx.QueryRow(ctx,
			`SELECT status FROM intentions WHERE dedupe_key = $1 AND status IN ('pending', 'running')`,
			dedupe).Scan(&dup.LiveStatus); serr != nil {
			dup.LiveStatus = "unknown"
		}
		return 0, false, dup, nil
	}
	return 0, false, nil, fmt.Errorf("применение: намерение не создано: %w", err)
}

// intentionDedupeKey — JSON-склейка инъективна для любых строк (паритет TS).
func intentionDedupeKey(kind, topic string, chatID int64) string {
	raw, _ := json.Marshal([]string{kind, strings.TrimSpace(strings.ToLower(topic)), fmt.Sprintf("%d", chatID)})
	return string(raw)
}

// deferTopic — тема отложенного ответа: последний текст пачки, обрезанный до 80 символов (паритет TS).
func deferTopic(ctx context.Context, tx pgx.Tx, turnID int64) (string, error) {
	var topic *string
	err := tx.QueryRow(ctx, `
		SELECT text FROM social_events WHERE turn_id = $1 AND text IS NOT NULL ORDER BY id DESC LIMIT 1`,
		turnID).Scan(&topic)
	if err != nil && !isNoRows(err) {
		return "", fmt.Errorf("применение: тема defer не найдена: %w", err)
	}
	if topic == nil || strings.TrimSpace(*topic) == "" {
		return "отложенный ответ", nil
	}
	t := []rune(*topic)
	if len(t) > 80 {
		t = t[:80]
	}
	return string(t), nil
}

// appliedMoodDelta — сжатая дельта для аудит-следа (как применено).
func appliedMoodDelta(clamped MoodDelta) MoodDelta {
	return clamped
}

// resolveActorEntity находит или создаёт сущность человека-актора первого
// события пачки (§7.3: карточка человека в scope чата). Актора нет — nil.
func resolveActorEntity(ctx context.Context, tx pgx.Tx, turnID int64, scopeID string) (*memory.Entity, error) {
	var (
		actorID   *int64
		actorName *string
	)
	err := tx.QueryRow(ctx, `
		SELECT actor_id, actor_name FROM social_events
		WHERE turn_id = $1 AND actor_id IS NOT NULL ORDER BY id LIMIT 1`,
		turnID).Scan(&actorID, &actorName)
	if isNoRows(err) || (err == nil && actorID == nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("применение: актор пачки не найден: %w", err)
	}
	name := fmt.Sprintf("user:%d", *actorID)
	if actorName != nil && strings.TrimSpace(*actorName) != "" {
		name = *actorName
	}
	entity, err := (memory.EntityRepo{}).FindOrCreate(ctx, tx, "person", name, scopeID)
	if err != nil {
		return nil, fmt.Errorf("применение: сущность собеседника: %w", err)
	}
	return &entity, nil
}
