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

	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/internal/telegram"
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
	// SkippedSticker — метка стикера не нашлась в коллекции: мягко обнулена,
	// ход живёт (как мусорная reaction при answer, этап 25.4).
	SkippedSticker bool
	// SuppressedSticker — стикер мягко обнулён предохранителем частоты (28.1):
	// в последних ходах чата стикер уже уходил.
	SuppressedSticker bool
	// SavedStickerID — стикер из пачки сохранён в коллекцию (25.5); 0 — нет.
	SavedStickerID int64
	// DroppedStickerSave — save_sticker отброшен до записи (28.1): в пачке нет
	// стикера, которого не было бы в коллекции. Модель предлагала сохранение
	// в каждом ходе — runtime решает, что сохранять нечего.
	DroppedStickerSave bool
	// DuplicateIntentions — предложения модели, отклонённые живым дублем
	// (§14.1): причина дубля (какое намерение живо) видна оператору в логе.
	DuplicateIntentions []DuplicateIntention
	// RejectedIntentions — предложения с темой-мусором (26.2): намерение не
	// создаётся, но ход живёт — как дубль, §14.1.
	RejectedIntentions []RejectedIntention
	// RaisedIntentions — задержки, поднятые runtime до пола вида (26.2).
	RaisedIntentions []RaisedIntention
	// LimitedIntentions — инициативные предложения, не созданные из-за
	// исчерпанного дневного лимита чата (§14.3, этап 33.2): фон всё равно
	// отменил бы их, поэтому runtime не плодит мёртвые строки.
	LimitedIntentions []LimitedIntention
	// SkippedReplies — реплаи, не отправленные из-за неизвестной/чужой метки
	// (37.2): сообщение уходит без реплая, причина — в лог оператора.
	SkippedReplies []SkippedReply
	// SkippedForward — пересылка не создана (37.3): метка не найдена или
	// относится к другому чату; ход живёт, причина — в лог.
	SkippedForward *SkippedReply
	// ForwardOutboxID — id пузыря пересылки; 0 — пересылки не было.
	ForwardOutboxID int64
}

// SkippedReply — реплай, который не удалось привязать к сообщению этого чата.
type SkippedReply struct {
	Index  int
	Ref    string
	Reason string
}

// RejectedIntention — отклонённое предложение намерения с мусорной темой.
type RejectedIntention struct {
	Kind   string
	Topic  string
	Reason string
}

// RaisedIntention — задержка намерения, поднятая до runtime-пола вида.
type RaisedIntention struct {
	Kind  string
	Topic string
	From  float64
	To    float64
}

// LimitedIntention — инициативное намерение, не созданное из-за дневного
// лимита чата (§14.3, этап 33.2).
type LimitedIntention struct {
	Kind  string
	Topic string
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
	// PersonaBaseline — цель затухания настроения (§7.2): базовая линия
	// персоны или блок расписания дня на момент применения (этап 22).
	PersonaBaseline Mood
	// Journal — пишет deltas_applied + delivery_queued в этой же транзакции.
	Journal *TurnJournal
	// BoredomReset — разговорный ход сбрасывает скуку (этап 23.1) в той же
	// транзакции применения. Фон (инициативы/explore) флаг не ставит: их
	// находки сбрасывают шкалу отдельным событием BoredomExplore.
	BoredomReset bool
	// Now — время применения для производных шкал (скука): ход передаёт свои
	// инжектированные часы, нулевое — time.Now. Без этого сброс скуки писал бы
	// стенные часы мимо управляемых часов тестов/планировщика.
	Now time.Time
	// Extra — дополнительная работа ВНУТРИ транзакции применения: вызывается
	// после CAS хода и до записи журнала (§12.1: «либо всё, либо ничего»
	// распространяется и на связанное состояние, например CAS намерения §14.3 —
	// нужно фону 9.6, чтобы статус намерения и его доставка менялись атомарно).
	// Ошибка, оборачивающая ErrApplyAborted, откатывает транзакцию и возвращает
	// Applied=false (применение уже сделано другим победителем).
	Extra func(ctx context.Context, tx pgx.Tx) error
	// Image — скачанная картинка хода (36.2): ложится в outbox_images в этой
	// же транзакции, пузырь kind=photo — отдельным сообщением после текста.
	// nil — картинки нет.
	Image *OutboxImage
	// ImageNotes — описания картинок пачки (37.1), уже сопоставленные с
	// событиями этого хода: пишутся в social_events.image_note в той же
	// транзакции. Память из них не создаётся.
	ImageNotes []EventImageNote
}

// OutboxImage — байты картинки для доставки sendPhoto (этап 36.2): скачаны
// fetch-слоем §15 до транзакции применения и хранятся рядом со строкой outbox,
// поэтому retry и рестарт не требуют повторного скачивания.
type OutboxImage struct {
	SourceURL   string
	ContentType string
	Bytes       []byte
	// Alt — подпись картинки со страницы (37.1): история вспоминает
	// отправленную картинку как «[отправил картинку: alt]».
	Alt string
}

// ApplyTurn применяет валидированный результат в одной транзакции.
func ApplyTurn(ctx context.Context, pool *runtime.Pool, opts ApplyOptions) (ApplyResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("применение: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// --- CAS строки хода: повторное применение отклоняется (§12.1, инвариант 4);
	// терминальный провал (29.1) и пропуск тоже не «воскрешаются» применением.
	tag, err := tx.Exec(ctx,
		`UPDATE turns SET status = 'applied', applied_at = now()
		 WHERE id = $1 AND status NOT IN ('applied', 'failed', 'skipped')`, opts.TurnID)
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

	// Реплаи (37.2): метка → Telegram message_id только этого чата (событие
	// пользователя или доставленный ответ бота). Неизвестная метка — сообщение
	// уходит без реплая, ход живёт; чужая метка так же не проходит.
	replyTargets := make(map[int]int64, len(opts.Result.ReplyToByMessage))
	for _, reply := range opts.Result.ReplyToByMessage {
		messageID, ok, rerr := resolveMessageLabel(ctx, tx, opts.ChatID, reply.Ref)
		if rerr != nil {
			return ApplyResult{}, rerr
		}
		if !ok {
			res.SkippedReplies = append(res.SkippedReplies, SkippedReply{
				Index: reply.Index, Ref: reply.Ref, Reason: "метка не найдена в этом чате",
			})
			continue
		}
		replyTargets[reply.Index] = messageID
	}

	// --- описания картинок (37.1): в события ЭТОГО хода, не в память. ---
	for _, note := range opts.ImageNotes {
		if strings.TrimSpace(note.Note) == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE social_events SET image_note = $1, image_kind = $2
			WHERE id = $3 AND turn_id = $4`,
			strings.TrimSpace(note.Note), note.Kind, note.EventID, opts.TurnID); err != nil {
			return ApplyResult{}, fmt.Errorf("применение: описание картинки не сохранено: %w", err)
		}
	}

	// --- доставка (§11.2, §13.1): сообщения, реакция, стикер ---
	delivery := &DeliveryQueued{TurnID: opts.TurnID, ChatID: opts.ChatID}
	position := 0
	enqueue := func(payload map[string]any, queued *QueuedDelivery) error {
		id, err := enqueueOutbox(ctx, tx, opts.TurnID, opts.ChatID, position, payload)
		if err != nil {
			return err
		}
		queued.ID = id
		queued.Position = position
		res.OutboxIDs = append(res.OutboxIDs, id)
		delivery.Outbox = append(delivery.Outbox, *queued)
		position++
		return nil
	}
	switch opts.Result.Decision {
	case DecisionAnswer:
		for i, text := range opts.Result.Messages {
			payload := map[string]any{"kind": "message", "text": text}
			if target := replyTargets[i+1]; target > 0 {
				payload["reply_to_message_id"] = target
			}
			if err := enqueue(payload, &QueuedDelivery{Kind: "message", Text: text}); err != nil {
				return ApplyResult{}, err
			}
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
			} else if err := enqueue(map[string]any{
				"kind":              "reaction",
				"reaction":          opts.Result.Reaction,
				"target_message_id": target,
			}, &QueuedDelivery{Kind: "reaction", Reaction: opts.Result.Reaction}); err != nil {
				return ApplyResult{}, err
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
		} else if err := enqueue(map[string]any{
			"kind":              "reaction",
			"reaction":          opts.Result.Reaction,
			"target_message_id": target,
		}, &QueuedDelivery{Kind: "reaction", Reaction: opts.Result.Reaction}); err != nil {
			return ApplyResult{}, err
		}
	}

	// --- стикер (25.4): метка из панели → file_id из коллекции; неизвестная
	// метка мягко обнуляется, как мусорная реакция при answer. Стикер уходит
	// после сообщений и реакции этого хода. Предохранитель частоты (28.1):
	// стикер уже был в последних ходах чата — предложение мягко обнуляется.
	if opts.Result.Sticker != "" {
		if stickerLabelNone(opts.Result.Sticker) {
			// Модель заполнила поле «ничем» (null/none/пунктуация/«не
			// используется…»): это не запрос стикера, а шум формы. Тихо
			// пропускаем, чтобы журнал не считал каждый ход ошибкой (33.1).
		} else if sticker, err := NewStickerRepo().ByLabel(ctx, tx, opts.Result.Sticker); err != nil {
			return ApplyResult{}, err
		} else if sticker == nil {
			res.SkippedSticker = true
		} else if frequent, ferr := stickerTooFrequent(ctx, tx, opts.ChatID, opts.TurnID); ferr != nil {
			return ApplyResult{}, ferr
		} else if frequent {
			res.SuppressedSticker = true
		} else if err := enqueue(map[string]any{
			"kind":    "sticker",
			"file_id": sticker.FileID,
			"emoji":   sticker.Emoji,
		}, &QueuedDelivery{Kind: "sticker", FileID: sticker.FileID}); err != nil {
			return ApplyResult{}, err
		}
	}

	// --- «сохраню себе» (25.5): file_id берём из пачки, заметку — у модели;
	// дубли отсекаются по file_unique_id. Стикера, которого нет в коллекции, в
	// пачке нет — сохранять нечего: предложение отбрасывается до записи, ход
	// живёт (28.1).
	if opts.Result.SaveSticker != nil {
		savedID, dropped, err := saveStickerFromBatch(ctx, tx, opts.TurnID, opts.Result.SaveSticker.Note)
		if err != nil {
			return ApplyResult{}, err
		}
		res.SavedStickerID = savedID
		res.DroppedStickerSave = dropped
	}

	// --- картинка (36.2): отдельный пузырь после сообщений хода; байты — в
	// outbox_images той же транзакцией, чтобы доставка не зависела от сети.
	if opts.Image != nil && len(opts.Image.Bytes) > 0 {
		photo := &QueuedDelivery{Kind: "photo"}
		if err := enqueue(map[string]any{
			"kind":       "photo",
			"source_url": opts.Image.SourceURL,
			"alt":        opts.Image.Alt,
		}, photo); err != nil {
			return ApplyResult{}, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO outbox_images (outbox_id, content_type, bytes) VALUES ($1, $2, $3)`,
			photo.ID, opts.Image.ContentType, opts.Image.Bytes); err != nil {
			return ApplyResult{}, fmt.Errorf("применение: байты картинки не сохранены: %w", err)
		}
	}

	// --- пересылка (37.3): только сообщение ЭТОГО чата; пузырь после
	// сообщений/остальных действий хода. Неизвестная/чужая метка мягко
	// обнуляется с логом — ход живёт.
	if strings.TrimSpace(opts.Result.Forward) != "" {
		messageID, ok, ferr := resolveMessageLabel(ctx, tx, opts.ChatID, opts.Result.Forward)
		if ferr != nil {
			return ApplyResult{}, ferr
		}
		if !ok {
			res.SkippedForward = &SkippedReply{
				Index: 0, Ref: opts.Result.Forward, Reason: "метка не найдена в этом чате",
			}
		} else {
			forward := &QueuedDelivery{Kind: "forward"}
			if err := enqueue(map[string]any{
				"kind":               "forward",
				"forward_message_id": messageID,
			}, forward); err != nil {
				return ApplyResult{}, err
			}
			res.ForwardOutboxID = forward.ID
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
	// Дневной лимит инициатив чата (§14.3, этап 33.2): тот же расчёт, что гейт
	// фона. Исчерпан — новые check_in/share/reply_later не создаём: фон отменил
	// бы их, а живая база копила десятки мёртвых строк за сутки. Исключение —
	// одна инициатива, если в чате нет ни одной ожидающей: фон при наступлении
	// срока перенесёт её на следующий слот (dailyLimitDeferral), иначе после
	// дня с двумя инициативами наступает тишина, пока человек сам не напишет.
	initiativeLimitReached := false
	if chatType, cerr := ChatTypeOf(ctx, tx, opts.ChatID); cerr != nil {
		return ApplyResult{}, cerr
	} else if chatType == "private" {
		usage, uerr := ReadInitiativeUsage(ctx, tx, opts.ChatID)
		if uerr != nil {
			return ApplyResult{}, uerr
		}
		initiativeLimitReached = InitiativeDailyExhausted(usage)
	}
	limitSlotReserved := false
	for _, proposal := range opts.Result.Intentions {
		// Тема-мусор (26.2) отклоняется до создания строки: «placeholder1»,
		// «:)» — не отложенное действие, а мусор в поле. Ход живёт, как при
		// дубле (§14.1).
		if reason := garbageTopicReason(proposal.Topic); reason != "" {
			res.RejectedIntentions = append(res.RejectedIntentions, RejectedIntention{
				Kind: proposal.Kind, Topic: proposal.Topic, Reason: reason,
			})
			continue
		}
		if initiativeLimitReached && IsInitiativeKind(proposal.Kind) {
			if limitSlotReserved {
				res.LimitedIntentions = append(res.LimitedIntentions, LimitedIntention{
					Kind: proposal.Kind, Topic: proposal.Topic,
				})
				continue
			}
			pending, perr := hasPendingMessageIntention(ctx, tx, opts.ChatID)
			if perr != nil {
				return ApplyResult{}, perr
			}
			if pending {
				res.LimitedIntentions = append(res.LimitedIntentions, LimitedIntention{
					Kind: proposal.Kind, Topic: proposal.Topic,
				})
				continue
			}
			// Свободного слота нет, но очередь пуста — эту инициативу ставим;
			// фон перенесёт её на следующий день, когда упрётся в лимит.
		}
		// Runtime-пол задержки по виду (26.2): модель путает часы с минутами,
		// runtime поднимает до пола, а не отклоняет намерение.
		flooredDelay, floored := floorIntentionDelay(proposal.Kind, proposal.DelayHours)
		var raised *RaisedIntention
		if floored {
			raised = &RaisedIntention{Kind: proposal.Kind, Topic: proposal.Topic,
				From: proposal.DelayHours, To: flooredDelay}
		}
		id, created, dup, err := createIntention(ctx, tx, intentionRow{
			kind:          proposal.Kind,
			chatID:        opts.ChatID,
			scopeID:       opts.ScopeID,
			topic:         proposal.Topic,
			reason:        proposal.Reason,
			delayHours:    flooredDelay,
			relatedTurnID: opts.TurnID,
		})
		if err != nil {
			return ApplyResult{}, err
		}
		if created {
			res.IntentionIDs = append(res.IntentionIDs, id)
			deltas.IntentionIDs = append(deltas.IntentionIDs, id)
			if initiativeLimitReached && IsInitiativeKind(proposal.Kind) {
				limitSlotReserved = true
			}
			if raised != nil {
				res.RaisedIntentions = append(res.RaisedIntentions, *raised)
			}
		} else if dup != nil {
			res.DuplicateIntentions = append(res.DuplicateIntentions, *dup)
		}
	}
	if opts.Result.Decision == DecisionDefer {
		if initiativeLimitReached {
			// reply_later — инициатива по §14.3 (гейт фона считает его так же):
			// при исчерпанном лимите не создаём и его.
			res.LimitedIntentions = append(res.LimitedIntentions, LimitedIntention{
				Kind: "reply_later", Topic: "отложенный ответ",
			})
		} else {
			topic, err := deferTopic(ctx, tx, opts.TurnID)
			if err != nil {
				return ApplyResult{}, err
			}
			// defer модельный: тот же runtime-пол вида reply_later (26.2).
			flooredDefer, floored := floorIntentionDelay("reply_later", *opts.Result.DeferHours)
			var raisedDefer *RaisedIntention
			if floored {
				raisedDefer = &RaisedIntention{Kind: "reply_later", Topic: topic,
					From: *opts.Result.DeferHours, To: flooredDefer}
			}
			id, created, dup, err := createIntention(ctx, tx, intentionRow{
				kind:          "reply_later",
				chatID:        opts.ChatID,
				scopeID:       opts.ScopeID,
				topic:         topic,
				reason:        "decision=defer: персонаж отложил ответ (§11.2)",
				delayHours:    flooredDefer,
				relatedTurnID: opts.TurnID,
			})
			if err != nil {
				return ApplyResult{}, err
			}
			if created {
				res.IntentionIDs = append(res.IntentionIDs, id)
				deltas.IntentionIDs = append(deltas.IntentionIDs, id)
				if raisedDefer != nil {
					res.RaisedIntentions = append(res.RaisedIntentions, *raisedDefer)
				}
			} else if dup != nil {
				res.DuplicateIntentions = append(res.DuplicateIntentions, *dup)
			}
		}
	}

	// --- скука (этап 23.1): разговорный ход — сильный сброс, в этой же
	// транзакции. Стоит после намерений, перед настроением: порядок блокировок
	// (intentions → character_state) совпадает с путём фона (applyExplore),
	// и первый же разговор не считает дельту от дефолтов таблицы.
	if opts.BoredomReset {
		now := opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		if _, err := NewBoredomRepo().ApplyEvent(ctx, tx, BoredomConversation,
			now, nil, nil, opts.PersonaBaseline); err != nil {
			return ApplyResult{}, fmt.Errorf("применение: сброс скуки: %w", err)
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

// resolveMessageLabel — метка #m<id> → Telegram message_id, но только если
// сообщение принадлежит ЭТОМУ чату (37.2, §9.1): событие пользователя или
// подтверждённо доставленный ответ бота. Метка из другого чата не проходит —
// реплай между чатами запрещён.
func resolveMessageLabel(ctx context.Context, tx pgx.Tx, chatID int64, raw string) (int64, bool, error) {
	messageID, ok := ParseMessageLabel(raw)
	if !ok {
		return 0, false, nil
	}
	var found bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM social_events WHERE chat_id = $1 AND telegram_message_id = $2
		) OR EXISTS (
		  SELECT 1 FROM delivery_outbox
		  WHERE chat_id = $1 AND telegram_message_id = $2 AND status = 'sent'
		)`, chatID, messageID).Scan(&found)
	if err != nil {
		return 0, false, fmt.Errorf("применение: метка %q не проверена: %w", raw, err)
	}
	return messageID, found, nil
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
	// Чат заблокировал бота (38.7): новые инициативы в него не создаются —
	// доставка всё равно получит 403 (этап 38.7). Возвращаем «не создано» с
	// причиной, чтобы вызывающий не считал это сбоем применения хода.
	if blocked, err := telegram.ChatBlocked(ctx, tx, row.chatID); err != nil {
		return 0, false, nil, fmt.Errorf("применение: пометка чата %d не прочитана: %w", row.chatID, err)
	} else if blocked {
		return 0, false, &DuplicateIntention{
			Kind: row.kind, Topic: strings.TrimSpace(row.topic), LiveStatus: "blocked",
		}, nil
	}
	ref := fmt.Sprintf("turn:%d", row.relatedTurnID)
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(ctx, tx,
		memory.SourceJournalEvent, ref, []string{row.scopeID})
	if err != nil {
		return 0, false, nil, fmt.Errorf("применение: разрешение намерения: %w", err)
	}
	notBefore := time.Now().Add(time.Duration(row.delayHours * float64(time.Hour)))
	expiresAt := notBefore.Add(intentionTTLHours * time.Hour)
	dedupe := IntentionDedupeKey(row.kind, row.topic, row.chatID)

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

// IntentionDedupeKey — JSON-склейка инъективна для любых строк (паритет TS).
// Экспортирована фону (этап 23.2): explore от скуки создаётся runtime'ом, но
// должен совпадать по ключу с намерениями из разговора — одна тема не плодит
// два поиска.
func IntentionDedupeKey(kind, topic string, chatID int64) string {
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
	name := ""
	if actorName != nil {
		name = *actorName
	}
	entity, err := (memory.EntityRepo{}).EnsurePerson(ctx, tx, *actorID, name, scopeID)
	if err != nil {
		return nil, fmt.Errorf("применение: сущность собеседника: %w", err)
	}
	return &entity, nil
}

// saveStickerFromBatch сохраняет в коллекцию первый НЕзнакомый стикер пачки
// (25.5, 28.1): file_id/file_unique_id/эмодзи берёт runtime из события, модель
// даёт только заметку. Возвращает id сохранённого стикера и признак «сохранять
// было нечего» (все стикеры пачки уже в коллекции или их нет вовсе) — такой
// save_sticker отбрасывается до записи, а не создаёт видимость работы.
func saveStickerFromBatch(ctx context.Context, tx pgx.Tx, turnID int64, note string) (int64, bool, error) {
	rows, err := tx.Query(ctx,
		`SELECT raw_update FROM social_events WHERE turn_id = $1 ORDER BY id`, turnID)
	if err != nil {
		return 0, false, fmt.Errorf("применение: события пачки не читаются: %w", err)
	}
	var stickers []telegram.StickerInfo
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("применение: событие пачки не читается: %w", err)
		}
		if info := telegram.StickerFromUpdate(raw); info != nil {
			stickers = append(stickers, *info)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("применение: события пачки не дочитаны: %w", err)
	}
	repo := NewStickerRepo()
	for _, sticker := range stickers {
		known, kerr := repo.Known(ctx, tx, sticker)
		if kerr != nil {
			return 0, false, kerr
		}
		if known {
			continue // этот уже в коллекции — ищем следующий незнакомый
		}
		id, created, serr := repo.Save(ctx, tx, sticker, note)
		if serr != nil {
			return 0, false, fmt.Errorf("применение: стикер не сохранён: %w", serr)
		}
		if !created {
			continue // дубль file_unique_id отсечён — пробуем следующий
		}
		return id, false, nil
	}
	return 0, len(stickers) > 0, nil
}

// hasPendingMessageIntention — есть ли в чате ожидающее сообщение-намерение
// (reply_later/check_in/share). Нужно apply-гейту лимита: одну инициативу
// ставим в очередь, только если другой уже нет, иначе за день копится
// несколько мёртвых строк. Explore сообщений не шлёт и слот не занимает.
func hasPendingMessageIntention(ctx context.Context, tx pgx.Tx, chatID int64) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM intentions
		  WHERE target_chat_id = $1 AND status = 'pending'
		    AND kind IN ('reply_later', 'check_in', 'share')
		)`, chatID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("применение: ожидающие намерения чата не прочитаны: %w", err)
	}
	return exists, nil
}
