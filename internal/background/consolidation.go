// Консолидация (§10.2): фоновая работа создаёт краткие episode summaries и
// актуализирует stances. Модель только ПРЕДЛАГАЕТ (proposal-контракт), применяет
// runtime — одной транзакцией: summary-запись, отметка summarized_at, пересмотры
// stances с основаниями, выбранными runtime из памяти эпизода. Повторное чтение
// тех же источников не повышает strength и не создаёт дубль summary (§10.3).
// Консолидация не делает источник снова активным и не превращает предположение
// в подтверждённый факт: summary — experience без confidence (salience 0.3).
package background

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agotask "ago/task"

	"github.com/luckystrker/dio-agent/internal/memory"
)

// consolidationCooldown — пауза консолидации эпизода после терминального сбоя
// (невалидный proposal после repair): без неё каждый такт сжигал бы бюджет на
// одном и том же эпизоде. Хранится в runtime_state (§18: maintenance cursor).
const consolidationCooldown = 24 * time.Hour

// consolidationOutcome — итог исполнения (для записи хода §20).
type consolidationOutcome struct {
	Outcome string // completed | deferred | failed | noop | error
	Detail  string
	Stats   modelStats
	Latency time.Duration
}

// runConsolidation исполняет консолидацию эпизода как ago-задачу: шаги
// collect → generate → apply с WAL и чекпоинтом (§12.2).
func (s *Scheduler) runConsolidation(ctx context.Context, ep Episode) (consolidationOutcome, error) {
	started := s.now()
	unitID := consolidationUnitID(ep.ChatID, ep.LastEventID)
	out := consolidationOutcome{}

	task, cp, err := s.startTask(ctx, unitID,
		fmt.Sprintf("Консолидация эпизода чата %d (%d событий)", ep.ChatID, len(ep.EventIDs)),
		map[string]any{"chat_id": ep.ChatID, "last_event": ep.LastEventID})
	if err != nil {
		return out, err
	}
	defer func() {
		if task.State() == agotask.StateRunning {
			_ = task.Fail(errors.New("исполнение вышло без терминального перехода"))
		}
	}()

	records, err := s.wal.Steps(ctx, unitID)
	if err != nil {
		return out, fmt.Errorf("фон: WAL задачи %q не читается: %w", unitID, err)
	}
	done := walDoneMap(records)

	// --- шаг collect: состав эпизода зафиксирован как точка восстановления ---
	if !done[stepCollect] {
		if err := s.wal.PlanStep(ctx, unitID, stepCollect, "consolidate_collect",
			map[string]any{"chat_id": ep.ChatID, "last_event": ep.LastEventID}); err != nil {
			return out, err
		}
		if err := s.wal.StepDone(ctx, unitID, stepCollect,
			map[string]any{"event_ids": int64sToAny(ep.EventIDs), "event_count": len(ep.EventIDs)}); err != nil {
			return out, err
		}
	}

	// --- шаг generate: модель предлагает summary и пересмотры stances ---
	var proposal consolidationProposal
	if !done[stepGenerate] {
		// §5.2: прямой шаг приоритетнее — консолидация откладывается целиком;
		// захвата не нужно: эпизод перечитывается каждым тактом.
		pending, perr := s.directPending(ctx)
		if perr != nil {
			return out, perr
		}
		if pending {
			_ = task.Cancel("отложено: прямое обращение в очереди")
			out.Outcome, out.Detail = "deferred", "прямое обращение в очереди (§5.2)"
			out.Latency = s.now().Sub(started)
			return out, nil
		}

		cp.Attempt++
		if err := s.saveCheckpoint(ctx, unitID, cp); err != nil {
			return out, err
		}
		op := fmt.Sprintf("model:bg:consolidate:%d:%d:%d", ep.ChatID, ep.LastEventID, cp.Attempt)
		if err := s.wal.PlanStep(ctx, unitID, stepGenerate, "consolidate_propose",
			map[string]any{"attempt": cp.Attempt}); err != nil {
			return out, err
		}

		system, userText, err := s.consolidationPrompt(ctx, ep)
		if err != nil {
			return out, err
		}
		p, stats, gerr := s.runConsolidationModel(ctx, op, system, userText)
		out.Stats = stats
		if gerr != nil {
			var invalid *errInvalidResult
			if errors.As(gerr, &invalid) {
				// Терминальный сбой: пауза на cooldown, иначе каждый такт
				// повторяет невалидный вызов и сжигает бюджет (§16).
				if cerr := s.setConsolidationCooldown(ctx, ep); cerr != nil {
					s.logError("фон: cooldown консолидации %q не записан: %v", unitID, cerr)
				}
				_ = s.wal.StepDone(ctx, unitID, stepGenerate, map[string]any{"failed": true})
				_ = task.Fail(invalid)
				out.Outcome, out.Detail = "failed", invalid.Error()
				out.Latency = s.now().Sub(started)
				return out, nil
			}
			return out, fmt.Errorf("фон: генерация консолидации %q не удалась: %w", unitID, gerr)
		}

		raw, merr := json.Marshal(p)
		if merr != nil {
			return out, merr
		}
		proposal = p
		// §12.2: результат шага — в WAL до применения: сбой между шагами
		// восстанавливает применение без новой генерации.
		if err := s.wal.StepDone(ctx, unitID, stepGenerate,
			map[string]any{"proposal": json.RawMessage(raw)}); err != nil {
			return out, err
		}
	} else {
		raw, ok := walProposal(records, stepGenerate)
		if !ok {
			return out, fmt.Errorf("фон: консолидация %q: generate отмечен, но proposal в WAL нет", unitID)
		}
		if proposal, err = parseConsolidationProposal(raw); err != nil {
			return out, fmt.Errorf("фон: сохранённый proposal консолидации %q не валиден: %w", unitID, err)
		}
	}

	// --- шаг apply: runtime применяет предложение одной транзакцией ---
	if !done[stepApply] {
		if err := s.wal.PlanStep(ctx, unitID, stepApply, "consolidate_apply",
			map[string]any{"events": len(ep.EventIDs)}); err != nil {
			return out, err
		}
		applied, detail, aerr := s.applyConsolidation(ctx, ep, proposal)
		if aerr != nil {
			return out, aerr // транзакция откачена; WAL видит apply без done — повтор
		}
		if !applied {
			out.Outcome, out.Detail = "noop", "эпизод уже обработан другим прогоном"
			out.Latency = s.now().Sub(started)
			return out, nil
		}
		out.Detail = detail
		if err := s.wal.StepDone(ctx, unitID, stepApply, map[string]any{"applied": true}); err != nil {
			return out, err
		}
	}

	_ = task.Complete(map[string]any{"events": len(ep.EventIDs)})
	out.Outcome = "completed"
	out.Latency = s.now().Sub(started)
	return out, nil
}

// consolidationEvent — событие эпизода (текст и ход-источник).
type consolidationEvent struct {
	ID         int64
	TurnID     *int64
	OccurredAt time.Time
	Actor      string
	Text       string
}

// consolidationPrompt собирает материалы эпизода: события, связанные записи
// памяти и сущности (имена — единственный словарь модели для stances).
func (s *Scheduler) consolidationPrompt(ctx context.Context, ep Episode) (system, userText string, err error) {
	scope := memory.ChatScope(fmt.Sprintf("%d", ep.ChatID))

	events, err := episodeEvents(ctx, s.pool, ep.EventIDs)
	if err != nil {
		return "", "", err
	}
	turnRefs := turnRefsOf(events)

	// Записи памяти эпизода: производные от ходов этих событий (источник —
	// ссылка turn:<id>); аудитория — скоуп чата (свой скоуп видит своё, §9.2).
	items, err := episodeMemoryItems(ctx, s.pool, s.pool, scope, turnRefs, episodeItemLimit)
	if err != nil {
		return "", "", err
	}

	// Сущности эпизода — только они могут получать stances: неизвестная ссылка
	// отклоняется runtime (§10.1).
	entities, err := episodeEntities(ctx, s.pool, turnRefs)
	if err != nil {
		return "", "", err
	}

	var b strings.Builder
	b.WriteString("Ты — фоновый модуль консолидации памяти персонажа. Сведи эпизод разговора в краткое резюме и предложи уточнения отношений (stances) к сущностям эпизода. Пиши по-русски, сжато и фактично.")
	b.WriteString("\n\nЭпизод чата, события:\n")
	for _, ev := range events {
		when := ev.OccurredAt.Format("02.01 15:04")
		text := truncateRunes(ev.Text, episodeTextLimit)
		if ev.Actor != "" {
			b.WriteString(fmt.Sprintf("[%s] %s: %s\n", when, ev.Actor, text))
		} else {
			b.WriteString(fmt.Sprintf("[%s] %s\n", when, text))
		}
	}
	if len(entities) > 0 {
		b.WriteString("\nСущности эпизода (поле entity у stance — только эти имена):\n")
		for _, e := range entities {
			b.WriteString(fmt.Sprintf("- %s\n", e.Name))
		}
	}
	if len(items) > 0 {
		b.WriteString("\nУже записанная память эпизода:\n")
		for _, item := range items {
			b.WriteString(fmt.Sprintf("- (%s) %s\n", item.Kind, truncateRunes(item.Text, episodeTextLimit)))
		}
	}
	return b.String(), "", nil
}

// applyConsolidation применяет proposal одной транзакцией: summary-запись,
// отметка событий summarized_at, пересмотры stances. Повторное применение
// невозможно: если хоть одно событие уже summarized — эпизод обработан (no-op).
func (s *Scheduler) applyConsolidation(ctx context.Context, ep Episode, p consolidationProposal) (bool, string, error) {
	scope := memory.ChatScope(fmt.Sprintf("%d", ep.ChatID))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, "", fmt.Errorf("консолидация: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var already int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NOT NULL`,
		ep.EventIDs).Scan(&already); err != nil {
		return false, "", fmt.Errorf("консолидация: отметки эпизода не проверяются: %w", err)
	}
	if already > 0 {
		return false, "", nil // обработано другим прогоном; транзакция откатится
	}

	// Источник и разрешение назначает runtime (§9.1): summary — производное
	// runtime-действия над эпизодом.
	ref := fmt.Sprintf("episode:%d:%d", ep.ChatID, ep.LastEventID)
	source := memory.SourceRef{Kind: memory.SourceRuntimeAction, Ref: ref}
	if _, err := (memory.SourceRepo{}).EnsureSource(ctx, tx, source); err != nil {
		return false, "", fmt.Errorf("консолидация: источник summary: %w", err)
	}
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(ctx, tx,
		memory.SourceRuntimeAction, ref, []string{scope})
	if err != nil {
		return false, "", fmt.Errorf("консолидация: разрешение summary: %w", err)
	}
	// experience без confidence: summary не превращает предположение в факт
	// (§10.2), повторное чтение источников уверенность не повышает (§10.3).
	if _, err := (memory.Items{}).Insert(ctx, tx, memory.NewItem{
		Kind:         "experience",
		Text:         p.Summary,
		Salience:     0.3,
		ProvenanceID: provenanceID,
		Source:       &source,
	}, scope); err != nil {
		return false, "", fmt.Errorf("консолидация: summary не записано: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE social_events SET summarized_at = now() WHERE id = ANY($1) AND summarized_at IS NULL`,
		ep.EventIDs); err != nil {
		return false, "", fmt.Errorf("консолидация: события не помечены: %w", err)
	}

	// Stances: модель предложила — runtime применяет (§10.2): имена резолвит по
	// сущностям эпизода, основания выбирает из памяти эпизода (§10.1),
	// повторное чтение тех же источников не поднимает strength (§10.3).
	turnRefs := episodeTurnRefs(ctx, tx, ep.EventIDs)
	entities, err := episodeEntities(ctx, tx, turnRefs)
	if err != nil {
		return false, "", err
	}
	byName := map[string]int64{}
	for _, e := range entities {
		byName[strings.ToLower(e.Name)] = e.ID
	}

	revised, skipped := 0, 0
	for _, sp := range p.Stances {
		entityID, ok := byName[strings.ToLower(sp.Entity)]
		if !ok {
			skipped++ // неизвестная сущность отклоняется (§10.1), остальные применяются
			continue
		}
		// §10.3: то же резюме — не ревизия: повторное чтение источников
		// не повышает strength и не крутит revision.
		st, serr := stanceOf(ctx, tx, entityID, sp.Aspect, scope)
		if serr != nil {
			return false, "", serr
		}
		if st != nil && st.Summary == sp.Summary {
			skipped++
			continue
		}
		basis, berr := episodeBasis(ctx, tx, entityID, turnRefs)
		if berr != nil {
			return false, "", berr
		}
		if _, err := (memory.Stances{}).Upsert(ctx, tx, memory.StanceInput{
			EntityID:       entityID,
			Aspect:         sp.Aspect,
			ScopeID:        scope,
			Summary:        sp.Summary,
			Strength:       sp.Strength,
			Interest:       sp.Interest,
			BasisMemoryIDs: basis,
		}); err != nil {
			return false, "", fmt.Errorf("консолидация: stance %q не применён: %w", sp.Entity, err)
		}
		revised++
	}

	if err := tx.Commit(ctx); err != nil {
		return false, "", fmt.Errorf("консолидация: транзакция не зафиксирована: %w", err)
	}
	detail := fmt.Sprintf("событий=%d stances=%d отклонено=%d", len(ep.EventIDs), revised, skipped)
	return true, detail, nil
}

// --- выборки эпизода ----------------------------------------------------------

// episodeEvents читает события эпизода (id известны из детектора).
func episodeEvents(ctx context.Context, q queryer, ids []int64) ([]consolidationEvent, error) {
	rows, err := q.Query(ctx, `
		SELECT id, turn_id, occurred_at, coalesce(actor_name, ''), coalesce(text, '')
		FROM social_events
		WHERE id = ANY($1)
		ORDER BY occurred_at, id`, ids)
	if err != nil {
		return nil, fmt.Errorf("консолидация: события эпизода не читаются: %w", err)
	}
	defer rows.Close()
	var out []consolidationEvent
	for rows.Next() {
		var ev consolidationEvent
		if err := rows.Scan(&ev.ID, &ev.TurnID, &ev.OccurredAt, &ev.Actor, &ev.Text); err != nil {
			return nil, fmt.Errorf("консолидация: строка события не читается: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func turnRefsOf(events []consolidationEvent) []string {
	seen := map[string]bool{}
	var refs []string
	for _, ev := range events {
		if ev.TurnID == nil {
			continue
		}
		ref := fmt.Sprintf("turn:%d", *ev.TurnID)
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	return refs
}

// episodeTurnRefs — ходы эпизода по id событий (применение знает только ids).
func episodeTurnRefs(ctx context.Context, q queryer, ids []int64) []string {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT 'turn:' || turn_id
		FROM social_events
		WHERE id = ANY($1) AND turn_id IS NOT NULL`, ids)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil
		}
		refs = append(refs, ref)
	}
	return refs
}

// episodeMemoryItems — записи памяти, производные от ходов эпизода, для
// аудитории скоупа чата. ID выбираются SQL-join'ом по источникам, тексты —
// через Items.Get с аудиторией: фильтр приватности §9.2 до выдачи модели,
// тот же код, что и у retrieval-инструментов (невидимое неотличимо от
// несуществующего).
func episodeMemoryItems(ctx context.Context, q queryer, pool *pgxpool.Pool, scope string, turnRefs []string, limit int) ([]memory.Item, error) {
	if len(turnRefs) == 0 {
		return nil, nil
	}
	// Пул кандидатов втрое шире лимита: невидимые отсеются на фильтре.
	rows, err := q.Query(ctx, `
		SELECT ms.memory_id
		FROM memory_sources ms
		JOIN sources s ON s.id = ms.source_id AND s.kind = 'journal_event' AND s.ref = ANY($1)
		JOIN memory_items m ON m.id = ms.memory_id
		ORDER BY m.recorded_at DESC
		LIMIT $2`, turnRefs, limit*3)
	if err != nil {
		return nil, fmt.Errorf("консолидация: записи эпизода не читаются: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("консолидация: строка записи не читается: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("консолидация: записи не дочитаны: %w", err)
	}

	var out []memory.Item
	for _, id := range ids {
		item, err := (memory.Items{}).Get(ctx, pool, id, scope)
		if errors.Is(err, memory.ErrNotFound) {
			continue // невидимое записи — как несуществующее (§9.2)
		}
		if err != nil {
			return nil, fmt.Errorf("консолидация: запись %d не читается: %w", id, err)
		}
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// episodeEntityRow — сущность эпизода для промпта и резолюции имён.
type episodeEntityRow struct {
	ID   int64
	Name string
}

// episodeEntities — сущности, связанные с записями эпизода.
func episodeEntities(ctx context.Context, q queryer, turnRefs []string) ([]episodeEntityRow, error) {
	if len(turnRefs) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT DISTINCT e.id, e.canonical_name
		FROM entities e
		JOIN memory_links ml ON ml.entity_id = e.id
		JOIN memory_sources ms ON ms.memory_id = ml.memory_id
		JOIN sources s ON s.id = ms.source_id AND s.kind = 'journal_event' AND s.ref = ANY($1)
		ORDER BY e.canonical_name`, turnRefs)
	if err != nil {
		return nil, fmt.Errorf("консолидация: сущности эпизода не читаются: %w", err)
	}
	defer rows.Close()
	var out []episodeEntityRow
	for rows.Next() {
		var e episodeEntityRow
		if err := rows.Scan(&e.ID, &e.Name); err != nil {
			return nil, fmt.Errorf("консолидация: строка сущности не читается: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// episodeBasis — основания пересмотра stance: записи памяти эпизода, связанные
// с сущностью (runtime выбирает сам — модель ссылок не предлагает, §10.1).
func episodeBasis(ctx context.Context, q queryer, entityID int64, turnRefs []string) ([]int64, error) {
	if len(turnRefs) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ml.memory_id
		FROM memory_links ml
		JOIN memory_sources ms ON ms.memory_id = ml.memory_id
		JOIN sources s ON s.id = ms.source_id
		WHERE ml.entity_id = $1 AND s.kind = 'journal_event' AND s.ref = ANY($2)
		ORDER BY ml.memory_id`, entityID, turnRefs)
	if err != nil {
		return nil, fmt.Errorf("консолидация: основания stance не читаются: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("консолидация: строка основания не читается: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// stanceOf читает текущий stance (nil — нет записи).
func stanceOf(ctx context.Context, q queryer, entityID int64, aspect, scope string) (*memory.Stance, error) {
	row := q.QueryRow(ctx, `
		SELECT id, entity_id, aspect, scope_id, summary, strength, interest, revision, updated_at
		FROM stances
		WHERE entity_id = $1 AND aspect = $2 AND scope_id = $3`, entityID, aspect, scope)
	var st memory.Stance
	err := row.Scan(&st.ID, &st.EntityID, &st.Aspect, &st.ScopeID, &st.Summary,
		&st.Strength, &st.Interest, &st.Revision, &st.UpdatedAt)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("консолидация: stance не читается: %w", err)
	}
	return &st, nil
}

// --- cooldown терминальных сбоев консолидации --------------------------------

func cooldownKey(ep Episode) string {
	return fmt.Sprintf("bg:consolidate:cooldown:%d:%d", ep.ChatID, ep.LastEventID)
}

// setConsolidationCooldown ставит паузу: эпизод не предлагается модели до
// истечения срока (иначе невалидный proposal повторялся бы каждым тактом).
func (s *Scheduler) setConsolidationCooldown(ctx context.Context, ep Episode) error {
	until := s.now().Add(consolidationCooldown)
	raw, err := json.Marshal(map[string]string{"until": until.UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO runtime_state (key, value) VALUES ($1, $2::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		cooldownKey(ep), string(raw))
	return err
}

// consolidationCooled — эпизод на паузе после терминального сбоя.
func (s *Scheduler) consolidationCooled(ctx context.Context, ep Episode) (bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM runtime_state WHERE key = $1`, cooldownKey(ep)).Scan(&raw)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("консолидация: cooldown %q не читается: %w", cooldownKey(ep), err)
	}
	var state struct {
		Until time.Time `json:"until"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return false, nil // битая запись не блокирует консолидацию
	}
	return s.now().Before(state.Until), nil
}

func int64sToAny(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func truncateRunes(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}
