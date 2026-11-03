// Объединение сущностей (спека §8.5, этап 32): модель предлагает «A = B» с
// основанием, runtime проверяет и склеивает одной транзакцией. Поглощённая
// сущность не удаляется: у неё остаётся merged_into, а журнал entity_merges
// хранит снимок перенесённых строк для отката.
//
// Проверки runtime (§8.5, §9.1, M7):
//   - обе сущности существуют и видны в скоупе эпизода;
//   - один скоуп приватности: личка с личкой, публичное с публичным, но не
//     личка с другим чатом/публичным — иначе склейка расширила бы доступ;
//   - разные Telegram user:<id> — разные люди, не склеиваются никогда;
//   - person не склеивается с не-person (и виды сущностей совпадают);
//   - сущность не склеивается сама с собой; повтор после склейки — no-op.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrMergeRefused — runtime отклонил предложенную склейку (правила §8.5).
var ErrMergeRefused = errors.New("memory: объединение сущностей отклонено")

// TxStarter — минимальный источник транзакций (runtime.Pool/pgxpool).
type TxStarter interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// EntityMergeRequest — предложение склеить сущности. Audience — скоуп
// эпизода, в котором обе сущности должны быть видимы.
type EntityMergeRequest struct {
	KeepID     int64
	MergeID    int64
	Reason     string
	ProposedBy string // consolidation | operator
	Audience   string
}

// EntityMergeResult — итог склейки.
type EntityMergeResult struct {
	KeepID        int64
	MergeID       int64
	JournalID     int64
	AlreadyMerged bool
}

// EntityUnmergeResult — итог отката.
type EntityUnmergeResult struct {
	JournalID int64
	KeepID    int64
	MergeID   int64
}

// mergeSnapshot — снимок перенесённых/удалённых строк для отката. Хранится
// JSONB в entity_merges.snapshot.
type mergeSnapshot struct {
	Keep               mergeEntitySnapshot   `json:"keep"`
	Merge              mergeEntitySnapshot   `json:"merge"`
	MergeAliases       []mergeAliasSnapshot  `json:"merge_aliases"`
	AddedAliasIDs      []int64               `json:"added_alias_ids"`
	MergeLinks         []mergeLinkSnapshot   `json:"merge_links"`
	MergeStances       []mergeStanceSnapshot `json:"merge_stances"`
	KeepStances        []mergeStanceSnapshot `json:"keep_stances"`
	MergeRelationships []mergeRelSnapshot    `json:"merge_relationships"`
	KeepRelationships  []mergeRelSnapshot    `json:"keep_relationships"`
}

type mergeEntitySnapshot struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	CanonicalName string `json:"canonical_name"`
	ScopeID       string `json:"scope_id"`
}

type mergeAliasSnapshot struct {
	ID      int64  `json:"id"`
	Alias   string `json:"alias"`
	ScopeID string `json:"scope_id"`
}

type mergeLinkSnapshot struct {
	MemoryID int64  `json:"memory_id"`
	Role     string `json:"role"`
}

type mergeStanceSnapshot struct {
	ID        int64     `json:"id"`
	Aspect    string    `json:"aspect"`
	ScopeID   string    `json:"scope_id"`
	Summary   string    `json:"summary"`
	Strength  float64   `json:"strength"`
	Interest  float64   `json:"interest"`
	Revision  int       `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
	Basis     []int64   `json:"basis"`
}

type mergeRelSnapshot struct {
	ScopeID     string    `json:"scope_id"`
	Familiarity float64   `json:"familiarity"`
	Affinity    float64   `json:"affinity"`
	Trust       float64   `json:"trust"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Merge открывает транзакцию и склеивает сущности атомарно.
func (EntityRepo) Merge(ctx context.Context, starter TxStarter, req EntityMergeRequest) (EntityMergeResult, error) {
	tx, err := starter.Begin(ctx)
	if err != nil {
		return EntityMergeResult{}, fmt.Errorf("объединение сущностей: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	res, err := (EntityRepo{}).MergeEntities(ctx, tx, req)
	if err != nil {
		return EntityMergeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EntityMergeResult{}, fmt.Errorf("объединение сущностей: транзакция не зафиксирована: %w", err)
	}
	return res, nil
}

// MergeEntities — склейка внутри уже открытой транзакции (консолидация:
// summary, stances и склейка применяются «всё или ничего»).
func (EntityRepo) MergeEntities(ctx context.Context, db DBTX, req EntityMergeRequest) (EntityMergeResult, error) {
	if req.KeepID <= 0 || req.MergeID <= 0 {
		return EntityMergeResult{}, fmt.Errorf("%w: не указаны обе сущности", ErrMergeRefused)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return EntityMergeResult{}, fmt.Errorf("%w: нет основания склейки", ErrMergeRefused)
	}
	if !IsValidScope(req.Audience) {
		return EntityMergeResult{}, fmt.Errorf("%w: %q", ErrInvalidScope, req.Audience)
	}
	proposedBy := strings.TrimSpace(req.ProposedBy)
	if proposedBy == "" {
		proposedBy = "operator"
	}

	originals, err := loadEntitiesForUpdate(ctx, db, req.KeepID, req.MergeID)
	if err != nil {
		return EntityMergeResult{}, err
	}
	keepOrig, ok := originals[req.KeepID]
	if !ok {
		return EntityMergeResult{}, fmt.Errorf("%w: сущность %d", ErrNotFound, req.KeepID)
	}
	mergeOrig, ok := originals[req.MergeID]
	if !ok {
		return EntityMergeResult{}, fmt.Errorf("%w: сущность %d", ErrNotFound, req.MergeID)
	}

	keepRoot, err := resolveEntityRoot(ctx, db, keepOrig)
	if err != nil {
		return EntityMergeResult{}, err
	}
	mergeRoot, err := resolveEntityRoot(ctx, db, mergeOrig)
	if err != nil {
		return EntityMergeResult{}, err
	}

	// Повтор предложения после успешной склейки — no-op: второй журнальной
	// записи и повторного переноса строк нет.
	if keepRoot.ID == mergeRoot.ID {
		if keepOrig.ID == mergeOrig.ID && keepOrig.MergedInto == nil {
			return EntityMergeResult{}, fmt.Errorf("%w: нельзя склеить сущность с самой собой", ErrMergeRefused)
		}
		return EntityMergeResult{KeepID: keepRoot.ID, MergeID: mergeOrig.ID, AlreadyMerged: true}, nil
	}
	if mergeOrig.MergedInto != nil {
		// Поглощённая сущность уже уехала в другую выжившую: цепочки не
		// разыгрываем — предложение отклоняется.
		return EntityMergeResult{}, fmt.Errorf("%w: сущность %d уже склеена с %d",
			ErrMergeRefused, mergeOrig.ID, *mergeOrig.MergedInto)
	}

	keep := keepRoot
	merge := mergeOrig

	// Обе сущности должны быть видимы в скоупе эпизода (§9.2: невидимое не
	// отличимо от несуществующего).
	if err := ensureEntityVisible(ctx, db, req.KeepID, req.Audience); err != nil {
		return EntityMergeResult{}, err
	}
	if err := ensureEntityVisible(ctx, db, req.MergeID, req.Audience); err != nil {
		return EntityMergeResult{}, err
	}

	if keep.ScopeID != merge.ScopeID {
		return EntityMergeResult{}, fmt.Errorf("%w: разные скоупы приватности (%q и %q)",
			ErrMergeRefused, keep.ScopeID, merge.ScopeID)
	}
	if keep.Kind != merge.Kind {
		return EntityMergeResult{}, fmt.Errorf("%w: разные виды сущностей (%q и %q)",
			ErrMergeRefused, keep.Kind, merge.Kind)
	}
	keepTelegram, err := entityTelegramIDs(ctx, db, keep)
	if err != nil {
		return EntityMergeResult{}, err
	}
	mergeTelegram, err := entityTelegramIDs(ctx, db, merge)
	if err != nil {
		return EntityMergeResult{}, err
	}
	if len(keepTelegram) > 0 && len(mergeTelegram) > 0 && !equalIDSet(keepTelegram, mergeTelegram) {
		return EntityMergeResult{}, fmt.Errorf("%w: разные Telegram-аккаунты (%s и %s)",
			ErrMergeRefused, joinIDs(keepTelegram), joinIDs(mergeTelegram))
	}

	snap, err := buildMergeSnapshot(ctx, db, keep, merge)
	if err != nil {
		return EntityMergeResult{}, err
	}

	if err := moveAliases(ctx, db, keep, merge, &snap); err != nil {
		return EntityMergeResult{}, err
	}
	if _, err := db.Exec(ctx,
		`UPDATE memory_links SET entity_id = $1 WHERE entity_id = $2`, keep.ID, merge.ID); err != nil {
		return EntityMergeResult{}, fmt.Errorf("объединение сущностей: связи памяти не перенесены: %w", err)
	}
	if err := moveStances(ctx, db, keep, merge, snap); err != nil {
		return EntityMergeResult{}, err
	}
	if err := moveRelationships(ctx, db, keep, merge, snap); err != nil {
		return EntityMergeResult{}, err
	}
	if _, err := db.Exec(ctx,
		`UPDATE entities SET merged_into = $1 WHERE id = $2`, keep.ID, merge.ID); err != nil {
		return EntityMergeResult{}, fmt.Errorf("объединение сущностей: пометка merged_into: %w", err)
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		return EntityMergeResult{}, fmt.Errorf("объединение сущностей: снимок не сериализован: %w", err)
	}
	var journalID int64
	if err := db.QueryRow(ctx, `
		INSERT INTO entity_merges
			(keep_id, merge_id, kind, scope_id, canonical_name, reason, proposed_by, snapshot)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		keep.ID, merge.ID, merge.Kind, merge.ScopeID, merge.CanonicalName,
		strings.TrimSpace(req.Reason), proposedBy, string(raw)).Scan(&journalID); err != nil {
		return EntityMergeResult{}, fmt.Errorf("объединение сущностей: журнал не записан: %w", err)
	}
	return EntityMergeResult{KeepID: keep.ID, MergeID: merge.ID, JournalID: journalID}, nil
}

// Unmerge открывает транзакцию и откатывает склейку по журналу.
func (EntityRepo) Unmerge(ctx context.Context, starter TxStarter, mergeID int64) (EntityUnmergeResult, error) {
	tx, err := starter.Begin(ctx)
	if err != nil {
		return EntityUnmergeResult{}, fmt.Errorf("откат объединения: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	res, err := (EntityRepo{}).UnmergeEntity(ctx, tx, mergeID)
	if err != nil {
		return EntityUnmergeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EntityUnmergeResult{}, fmt.Errorf("откат объединения: транзакция не зафиксирована: %w", err)
	}
	return res, nil
}

// UnmergeEntity — откат склейки внутри уже открытой транзакции. mergeID — id
// поглощённой сущности (не id журнальной записи).
func (EntityRepo) UnmergeEntity(ctx context.Context, db DBTX, mergeID int64) (EntityUnmergeResult, error) {
	var (
		journalID int64
		keepID    int64
		rawSnap   []byte
	)
	err := db.QueryRow(ctx, `
		SELECT id, keep_id, snapshot FROM entity_merges
		WHERE merge_id = $1 AND rolled_back_at IS NULL
		ORDER BY id DESC LIMIT 1 FOR UPDATE`, mergeID).
		Scan(&journalID, &keepID, &rawSnap)
	if isNoRows(err) {
		return EntityUnmergeResult{}, fmt.Errorf("%w: активная склейка сущности %d", ErrNotFound, mergeID)
	}
	if err != nil {
		return EntityUnmergeResult{}, fmt.Errorf("откат объединения: журнал не читается: %w", err)
	}

	entities, err := loadEntitiesForUpdate(ctx, db, keepID, mergeID)
	if err != nil {
		return EntityUnmergeResult{}, err
	}
	keep, ok := entities[keepID]
	if !ok {
		return EntityUnmergeResult{}, fmt.Errorf("%w: выжившая сущность %d", ErrNotFound, keepID)
	}
	merge, ok := entities[mergeID]
	if !ok {
		return EntityUnmergeResult{}, fmt.Errorf("%w: поглощённая сущность %d", ErrNotFound, mergeID)
	}
	if merge.MergedInto == nil || *merge.MergedInto != keepID {
		return EntityUnmergeResult{}, fmt.Errorf("%w: сущность %d не помечена склеенной с %d",
			ErrMergeRefused, mergeID, keepID)
	}
	if keep.MergedInto != nil {
		return EntityUnmergeResult{}, fmt.Errorf("%w: выжившая сущность %d уже склеена дальше; откат цепочки не поддержан",
			ErrMergeRefused, keepID)
	}

	var snap mergeSnapshot
	if err := json.Unmarshal(rawSnap, &snap); err != nil {
		return EntityUnmergeResult{}, fmt.Errorf("откат объединения: снимок журнала битый: %w", err)
	}

	if err := restoreAliases(ctx, db, keepID, mergeID, snap); err != nil {
		return EntityUnmergeResult{}, err
	}
	if err := restoreLinks(ctx, db, keepID, mergeID, snap); err != nil {
		return EntityUnmergeResult{}, err
	}
	if err := restoreStances(ctx, db, keepID, mergeID, snap); err != nil {
		return EntityUnmergeResult{}, err
	}
	if err := restoreRelationships(ctx, db, keepID, mergeID, snap); err != nil {
		return EntityUnmergeResult{}, err
	}

	if _, err := db.Exec(ctx,
		`UPDATE entities SET merged_into = NULL WHERE id = $1 AND merged_into = $2`, mergeID, keepID); err != nil {
		return EntityUnmergeResult{}, fmt.Errorf("откат объединения: merged_into не снят: %w", err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE entity_merges SET rolled_back_at = now() WHERE id = $1`, journalID); err != nil {
		return EntityUnmergeResult{}, fmt.Errorf("откат объединения: журнал не помечен: %w", err)
	}
	return EntityUnmergeResult{JournalID: journalID, KeepID: keepID, MergeID: mergeID}, nil
}

// --- загрузка и снимок --------------------------------------------------------

func loadEntitiesForUpdate(ctx context.Context, db DBTX, ids ...int64) (map[int64]Entity, error) {
	rows, err := db.Query(ctx, `
		SELECT id, kind, canonical_name, scope_id, merged_into
		FROM entities WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("объединение сущностей: сущности не читаются: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]Entity, len(ids))
	for rows.Next() {
		var e Entity
		if err := rows.Scan(&e.ID, &e.Kind, &e.CanonicalName, &e.ScopeID, &e.MergedInto); err != nil {
			return nil, fmt.Errorf("объединение сущностей: строка сущности не читается: %w", err)
		}
		out[e.ID] = e
	}
	return out, rows.Err()
}

func loadEntityForUpdate(ctx context.Context, db DBTX, id int64) (Entity, error) {
	entities, err := loadEntitiesForUpdate(ctx, db, id)
	if err != nil {
		return Entity{}, err
	}
	e, ok := entities[id]
	if !ok {
		return Entity{}, fmt.Errorf("%w: сущность %d", ErrNotFound, id)
	}
	return e, nil
}

func resolveEntityRoot(ctx context.Context, db DBTX, e Entity) (Entity, error) {
	seen := map[int64]bool{e.ID: true}
	for e.MergedInto != nil {
		next := *e.MergedInto
		if seen[next] {
			return Entity{}, fmt.Errorf("%w: цикл merged_into у сущности %d", ErrMergeRefused, e.ID)
		}
		seen[next] = true
		var err error
		e, err = loadEntityForUpdate(ctx, db, next)
		if err != nil {
			return Entity{}, err
		}
	}
	return e, nil
}

func ensureEntityVisible(ctx context.Context, db DBTX, id int64, audience string) error {
	if _, err := (EntityRepo{}).Get(ctx, db, id, audience); err != nil {
		return err
	}
	return nil
}

var telegramIDPattern = regexp.MustCompile(`(?i)^user:(\d+)$`)

func entityTelegramIDs(ctx context.Context, db DBTX, e Entity) (map[string]bool, error) {
	out := map[string]bool{}
	if m := telegramIDPattern.FindStringSubmatch(strings.TrimSpace(e.CanonicalName)); m != nil {
		out[m[1]] = true
	}
	rows, err := db.Query(ctx, `SELECT alias FROM entity_aliases WHERE entity_id = $1`, e.ID)
	if err != nil {
		return nil, fmt.Errorf("объединение сущностей: алиасы сущности не читаются: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, fmt.Errorf("объединение сущностей: строка алиаса не читается: %w", err)
		}
		if m := telegramIDPattern.FindStringSubmatch(strings.TrimSpace(alias)); m != nil {
			out[m[1]] = true
		}
	}
	return out, rows.Err()
}

func equalIDSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if !b[id] {
			return false
		}
	}
	return true
}

func joinIDs(ids map[string]bool) string {
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, "user:"+id)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func buildMergeSnapshot(ctx context.Context, db DBTX, keep, merge Entity) (mergeSnapshot, error) {
	snap := mergeSnapshot{
		Keep:  mergeEntitySnapshot{ID: keep.ID, Kind: keep.Kind, CanonicalName: keep.CanonicalName, ScopeID: keep.ScopeID},
		Merge: mergeEntitySnapshot{ID: merge.ID, Kind: merge.Kind, CanonicalName: merge.CanonicalName, ScopeID: merge.ScopeID},
	}
	var err error
	if snap.MergeAliases, err = loadAliasSnapshots(ctx, db, merge.ID); err != nil {
		return mergeSnapshot{}, err
	}
	if snap.MergeLinks, err = loadLinkSnapshots(ctx, db, merge.ID); err != nil {
		return mergeSnapshot{}, err
	}
	if snap.MergeStances, err = loadStanceSnapshots(ctx, db, merge.ID); err != nil {
		return mergeSnapshot{}, err
	}
	// Конфликтующие по (aspect, scope) stance выжившей сущности — их основания
	// пополнятся при склейке, поэтому снимок нужен для отката.
	keepStanceByPair := map[string]mergeStanceSnapshot{}
	for _, st := range snap.MergeStances {
		if _, loaded := keepStanceByPair[st.Aspect+"\x00"+st.ScopeID]; loaded {
			continue
		}
		stKeep, ok, err := loadStanceSnapshot(ctx, db, keep.ID, st.Aspect, st.ScopeID)
		if err != nil {
			return mergeSnapshot{}, err
		}
		if ok {
			keepStanceByPair[st.Aspect+"\x00"+st.ScopeID] = stKeep
			snap.KeepStances = append(snap.KeepStances, stKeep)
		}
	}
	if snap.MergeRelationships, err = loadRelationshipSnapshots(ctx, db, merge.ID, nil); err != nil {
		return mergeSnapshot{}, err
	}
	scopes := make([]string, 0, len(snap.MergeRelationships))
	for _, rel := range snap.MergeRelationships {
		scopes = append(scopes, rel.ScopeID)
	}
	if snap.KeepRelationships, err = loadRelationshipSnapshots(ctx, db, keep.ID, scopes); err != nil {
		return mergeSnapshot{}, err
	}
	return snap, nil
}

func loadAliasSnapshots(ctx context.Context, db DBTX, entityID int64) ([]mergeAliasSnapshot, error) {
	rows, err := db.Query(ctx, `
		SELECT id, alias, scope_id FROM entity_aliases
		WHERE entity_id = $1 ORDER BY id`, entityID)
	if err != nil {
		return nil, fmt.Errorf("объединение сущностей: алиасы не читаются: %w", err)
	}
	defer rows.Close()
	var out []mergeAliasSnapshot
	for rows.Next() {
		var a mergeAliasSnapshot
		if err := rows.Scan(&a.ID, &a.Alias, &a.ScopeID); err != nil {
			return nil, fmt.Errorf("объединение сущностей: строка алиаса не читается: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func loadLinkSnapshots(ctx context.Context, db DBTX, entityID int64) ([]mergeLinkSnapshot, error) {
	rows, err := db.Query(ctx, `
		SELECT memory_id, role FROM memory_links
		WHERE entity_id = $1 ORDER BY memory_id, role`, entityID)
	if err != nil {
		return nil, fmt.Errorf("объединение сущностей: связи памяти не читаются: %w", err)
	}
	defer rows.Close()
	var out []mergeLinkSnapshot
	for rows.Next() {
		var l mergeLinkSnapshot
		if err := rows.Scan(&l.MemoryID, &l.Role); err != nil {
			return nil, fmt.Errorf("объединение сущностей: строка связи не читается: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func loadStanceSnapshots(ctx context.Context, db DBTX, entityID int64) ([]mergeStanceSnapshot, error) {
	rows, err := db.Query(ctx, `
		SELECT id, aspect, scope_id, summary, strength, interest, revision, updated_at
		FROM stances WHERE entity_id = $1 ORDER BY id`, entityID)
	if err != nil {
		return nil, fmt.Errorf("объединение сущностей: stances не читаются: %w", err)
	}
	defer rows.Close()
	var out []mergeStanceSnapshot
	for rows.Next() {
		var st mergeStanceSnapshot
		if err := rows.Scan(&st.ID, &st.Aspect, &st.ScopeID, &st.Summary,
			&st.Strength, &st.Interest, &st.Revision, &st.UpdatedAt); err != nil {
			return nil, fmt.Errorf("объединение сущностей: строка stance не читается: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		basis, err := loadStanceBasis(ctx, db, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Basis = basis
	}
	return out, nil
}

func loadStanceSnapshot(ctx context.Context, db DBTX, entityID int64, aspect, scope string) (mergeStanceSnapshot, bool, error) {
	var st mergeStanceSnapshot
	err := db.QueryRow(ctx, `
		SELECT id, aspect, scope_id, summary, strength, interest, revision, updated_at
		FROM stances WHERE entity_id = $1 AND aspect = $2 AND scope_id = $3`,
		entityID, aspect, scope).Scan(&st.ID, &st.Aspect, &st.ScopeID, &st.Summary,
		&st.Strength, &st.Interest, &st.Revision, &st.UpdatedAt)
	if isNoRows(err) {
		return mergeStanceSnapshot{}, false, nil
	}
	if err != nil {
		return mergeStanceSnapshot{}, false, fmt.Errorf("объединение сущностей: stance не читается: %w", err)
	}
	basis, err := loadStanceBasis(ctx, db, st.ID)
	if err != nil {
		return mergeStanceSnapshot{}, false, err
	}
	st.Basis = basis
	return st, true, nil
}

func loadStanceBasis(ctx context.Context, db DBTX, stanceID int64) ([]int64, error) {
	rows, err := db.Query(ctx,
		`SELECT memory_id FROM stance_basis WHERE stance_id = $1 ORDER BY memory_id`, stanceID)
	if err != nil {
		return nil, fmt.Errorf("объединение сущностей: основания stance не читаются: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("объединение сущностей: строка основания не читается: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func loadRelationshipSnapshots(ctx context.Context, db DBTX, entityID int64, scopes []string) ([]mergeRelSnapshot, error) {
	query := `SELECT scope_id, familiarity, affinity, trust, updated_at
		FROM relationships WHERE person_entity_id = $1`
	args := []any{entityID}
	if scopes != nil {
		query += ` AND scope_id = ANY($2)`
		args = append(args, scopes)
	}
	query += ` ORDER BY scope_id`
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("объединение сущностей: отношения не читаются: %w", err)
	}
	defer rows.Close()
	var out []mergeRelSnapshot
	for rows.Next() {
		var rel mergeRelSnapshot
		if err := rows.Scan(&rel.ScopeID, &rel.Familiarity, &rel.Affinity, &rel.Trust, &rel.UpdatedAt); err != nil {
			return nil, fmt.Errorf("объединение сущностей: строка отношений не читается: %w", err)
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}

// --- перенос строк ------------------------------------------------------------

func moveAliases(ctx context.Context, db DBTX, keep, merge Entity, snap *mergeSnapshot) error {
	// Сначала добавляем каноническое имя merge алиасом keep, затем переносим
	// остальные; точные повторы (нормализованное имя уже есть у keep) удаляются.
	var addedID int64
	err := db.QueryRow(ctx, `
		INSERT INTO entity_aliases (entity_id, alias, scope_id)
		SELECT $1, $2, e.scope_id FROM entities e
		WHERE e.id = $1 AND NOT EXISTS (
			SELECT 1 FROM entity_aliases a
			WHERE a.entity_id = $1 AND `+entityKeySQL("a.alias")+` = `+entityKeySQL("$2")+`
		)
		RETURNING id`, keep.ID, merge.CanonicalName).Scan(&addedID)
	if err != nil && !isNoRows(err) {
		return fmt.Errorf("объединение сущностей: каноническое имя merge не стало алиасом: %w", err)
	}
	if err == nil {
		snap.AddedAliasIDs = []int64{addedID}
	}

	if _, err := db.Exec(ctx, `
		UPDATE entity_aliases m SET entity_id = $1
		WHERE m.entity_id = $2
		  AND NOT EXISTS (
			SELECT 1 FROM entity_aliases k
			WHERE k.entity_id = $1 AND `+entityKeySQL("k.alias")+` = `+entityKeySQL("m.alias")+`
		  )`, keep.ID, merge.ID); err != nil {
		return fmt.Errorf("объединение сущностей: алиасы не перенесены: %w", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM entity_aliases WHERE entity_id = $1`, merge.ID); err != nil {
		return fmt.Errorf("объединение сущностей: дубли алиасов не убраны: %w", err)
	}
	return nil
}

func moveStances(ctx context.Context, db DBTX, keep, merge Entity, snap mergeSnapshot) error {
	keepByPair := make(map[string]bool, len(snap.KeepStances))
	for _, st := range snap.KeepStances {
		keepByPair[st.Aspect+"\x00"+st.ScopeID] = true
	}
	for _, st := range snap.MergeStances {
		if keepByPair[st.Aspect+"\x00"+st.ScopeID] {
			// Конфликт: остаётся keep, основания merge добавляются к нему,
			// сам stance merge удаляется (его снимок — в журнале).
			for _, memoryID := range st.Basis {
				if _, err := db.Exec(ctx, `
					INSERT INTO stance_basis (stance_id, memory_id)
					SELECT s.id, $2 FROM stances s
					WHERE s.entity_id = $1 AND s.aspect = $3 AND s.scope_id = $4
					  AND NOT EXISTS (SELECT 1 FROM stance_basis b
					                  WHERE b.stance_id = s.id AND b.memory_id = $2)`,
					keep.ID, memoryID, st.Aspect, st.ScopeID); err != nil {
					return fmt.Errorf("объединение сущностей: основание stance не перенесено: %w", err)
				}
			}
			if _, err := db.Exec(ctx, `DELETE FROM stance_basis WHERE stance_id = $1`, st.ID); err != nil {
				return fmt.Errorf("объединение сущностей: основания stance merge не убраны: %w", err)
			}
			if _, err := db.Exec(ctx, `DELETE FROM stances WHERE id = $1`, st.ID); err != nil {
				return fmt.Errorf("объединение сущностей: stance merge не удалён: %w", err)
			}
			continue
		}
		if _, err := db.Exec(ctx, `UPDATE stances SET entity_id = $1 WHERE id = $2`, keep.ID, st.ID); err != nil {
			return fmt.Errorf("объединение сущностей: stance не перенесён: %w", err)
		}
	}
	return nil
}

func moveRelationships(ctx context.Context, db DBTX, keep, merge Entity, snap mergeSnapshot) error {
	keepByScope := make(map[string]mergeRelSnapshot, len(snap.KeepRelationships))
	for _, rel := range snap.KeepRelationships {
		keepByScope[rel.ScopeID] = rel
	}
	for _, rel := range snap.MergeRelationships {
		existing, conflict := keepByScope[rel.ScopeID]
		if conflict {
			// Правило конфликта: выживает строка keep; familiarity — max
			// (опыт не теряем), affinity/trust — середина между keep и merge
			// (чужой опыт не перевешивает, но и не выбрасывается).
			familiarity := existing.Familiarity
			if rel.Familiarity > familiarity {
				familiarity = rel.Familiarity
			}
			affinity := existing.Affinity + (rel.Affinity-existing.Affinity)*0.5
			trust := existing.Trust + (rel.Trust-existing.Trust)*0.5
			if _, err := db.Exec(ctx, `
				UPDATE relationships
				SET familiarity = $1, affinity = $2, trust = $3, updated_at = now()
				WHERE person_entity_id = $4 AND scope_id = $5`,
				familiarity, affinity, trust, keep.ID, rel.ScopeID); err != nil {
				return fmt.Errorf("объединение сущностей: отношения keep не слиты: %w", err)
			}
			if _, err := db.Exec(ctx,
				`DELETE FROM relationships WHERE person_entity_id = $1 AND scope_id = $2`,
				merge.ID, rel.ScopeID); err != nil {
				return fmt.Errorf("объединение сущностей: отношения merge не удалены: %w", err)
			}
			continue
		}
		if _, err := db.Exec(ctx, `
			UPDATE relationships SET person_entity_id = $1, updated_at = now()
			WHERE person_entity_id = $2 AND scope_id = $3`,
			keep.ID, merge.ID, rel.ScopeID); err != nil {
			return fmt.Errorf("объединение сущностей: отношения не перенесены: %w", err)
		}
	}
	return nil
}

// --- откат --------------------------------------------------------------------

func restoreAliases(ctx context.Context, db DBTX, keepID, mergeID int64, snap mergeSnapshot) error {
	if len(snap.AddedAliasIDs) > 0 {
		if _, err := db.Exec(ctx,
			`DELETE FROM entity_aliases WHERE id = ANY($1) AND entity_id = $2`,
			snap.AddedAliasIDs, keepID); err != nil {
			return fmt.Errorf("откат объединения: добавленный алиас не убран: %w", err)
		}
	}
	for _, a := range snap.MergeAliases {
		if _, err := db.Exec(ctx, `DELETE FROM entity_aliases WHERE id = $1`, a.ID); err != nil {
			return fmt.Errorf("откат объединения: алиас не очищен: %w", err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO entity_aliases (id, entity_id, alias, scope_id)
			VALUES ($1, $2, $3, $4)`, a.ID, mergeID, a.Alias, a.ScopeID); err != nil {
			return fmt.Errorf("откат объединения: алиас не возвращён: %w", err)
		}
	}
	return nil
}

func restoreLinks(ctx context.Context, db DBTX, keepID, mergeID int64, snap mergeSnapshot) error {
	for _, l := range snap.MergeLinks {
		tag, err := db.Exec(ctx, `
			DELETE FROM memory_links WHERE ctid = (
				SELECT ctid FROM memory_links
				WHERE memory_id = $1 AND entity_id = $2 AND role = $3
				LIMIT 1
			)`, l.MemoryID, keepID, l.Role)
		if err != nil {
			return fmt.Errorf("откат объединения: связь памяти не очищена: %w", err)
		}
		if tag.RowsAffected() == 0 {
			continue // связь удалена уже после склейки — не воскрешаем
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO memory_links (memory_id, entity_id, role)
			VALUES ($1, $2, $3)`, l.MemoryID, mergeID, l.Role); err != nil {
			return fmt.Errorf("откат объединения: связь памяти не возвращена: %w", err)
		}
	}
	return nil
}

func restoreStances(ctx context.Context, db DBTX, keepID, mergeID int64, snap mergeSnapshot) error {
	affectedAspects := make([]string, 0, len(snap.MergeStances))
	affectedScopes := make([]string, 0, len(snap.MergeStances))
	mergeIDs := make([]int64, 0, len(snap.MergeStances))
	for _, st := range snap.MergeStances {
		affectedAspects = append(affectedAspects, st.Aspect)
		affectedScopes = append(affectedScopes, st.ScopeID)
		mergeIDs = append(mergeIDs, st.ID)
	}

	// Основания текущих stances затронутых пар (и перенесённых stance merge)
	// удаляются до самих stances: FK stance_basis не каскадный.
	if len(affectedAspects) > 0 {
		if _, err := db.Exec(ctx, `
			DELETE FROM stance_basis WHERE stance_id IN (
				SELECT s.id FROM stances s
				WHERE s.entity_id = $1
				  AND (s.aspect, s.scope_id) IN (
					SELECT UNNEST($2::text[]), UNNEST($3::text[])
				  )
			)`, keepID, affectedAspects, affectedScopes); err != nil {
			return fmt.Errorf("откат объединения: основания keep не очищены: %w", err)
		}
	}
	if len(mergeIDs) > 0 {
		if _, err := db.Exec(ctx,
			`DELETE FROM stance_basis WHERE stance_id = ANY($1)`, mergeIDs); err != nil {
			return fmt.Errorf("откат объединения: основания merge не очищены: %w", err)
		}
	}
	if len(affectedAspects) > 0 {
		if _, err := db.Exec(ctx, `
			DELETE FROM stances
			WHERE entity_id = $1
			  AND (aspect, scope_id) IN (SELECT UNNEST($2::text[]), UNNEST($3::text[]))`,
			keepID, affectedAspects, affectedScopes); err != nil {
			return fmt.Errorf("откат объединения: stances keep не очищены: %w", err)
		}
	}
	if len(mergeIDs) > 0 {
		if _, err := db.Exec(ctx, `DELETE FROM stances WHERE id = ANY($1)`, mergeIDs); err != nil {
			return fmt.Errorf("откат объединения: stances merge не очищены: %w", err)
		}
	}

	insertStance := func(st mergeStanceSnapshot, entityID int64) error {
		if _, err := db.Exec(ctx, `
			INSERT INTO stances (id, entity_id, aspect, scope_id, summary, strength, interest, revision, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			st.ID, entityID, st.Aspect, st.ScopeID, st.Summary, st.Strength, st.Interest, st.Revision, st.UpdatedAt); err != nil {
			return err
		}
		for _, memoryID := range st.Basis {
			if _, err := db.Exec(ctx, `
				INSERT INTO stance_basis (stance_id, memory_id) VALUES ($1, $2)`,
				st.ID, memoryID); err != nil {
				return err
			}
		}
		return nil
	}
	for _, st := range snap.KeepStances {
		if err := insertStance(st, keepID); err != nil {
			return fmt.Errorf("откат объединения: stance keep не возвращён: %w", err)
		}
	}
	for _, st := range snap.MergeStances {
		if err := insertStance(st, mergeID); err != nil {
			return fmt.Errorf("откат объединения: stance merge не возвращён: %w", err)
		}
	}
	return nil
}

func restoreRelationships(ctx context.Context, db DBTX, keepID, mergeID int64, snap mergeSnapshot) error {
	scopes := make([]string, 0, len(snap.MergeRelationships)+len(snap.KeepRelationships))
	seen := map[string]bool{}
	for _, rel := range append(append([]mergeRelSnapshot{}, snap.KeepRelationships...), snap.MergeRelationships...) {
		if !seen[rel.ScopeID] {
			seen[rel.ScopeID] = true
			scopes = append(scopes, rel.ScopeID)
		}
	}
	if len(scopes) == 0 {
		return nil
	}
	if _, err := db.Exec(ctx, `
		DELETE FROM relationships
		WHERE person_entity_id IN ($1, $2) AND scope_id = ANY($3)`,
		keepID, mergeID, scopes); err != nil {
		return fmt.Errorf("откат объединения: отношения не очищены: %w", err)
	}
	insertRel := func(entityID int64, rel mergeRelSnapshot) error {
		_, err := db.Exec(ctx, `
			INSERT INTO relationships (person_entity_id, scope_id, familiarity, affinity, trust, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			entityID, rel.ScopeID, rel.Familiarity, rel.Affinity, rel.Trust, rel.UpdatedAt)
		return err
	}
	for _, rel := range snap.KeepRelationships {
		if err := insertRel(keepID, rel); err != nil {
			return fmt.Errorf("откат объединения: отношения keep не возвращены: %w", err)
		}
	}
	for _, rel := range snap.MergeRelationships {
		if err := insertRel(mergeID, rel); err != nil {
			return fmt.Errorf("откат объединения: отношения merge не возвращены: %w", err)
		}
	}
	return nil
}
