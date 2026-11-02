// Memory items (спека §8.2, §10): типизированные записи единой памяти,
// связи с сущностями (§8.3), пересмотр без тихой перезаписи (§10.3),
// извлечение с фильтром приватности до ранжирования (§9.2).
package memory

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Виды записей (§8.2) и статусы жизненного цикла.
const (
	KindClaim      = "claim"
	KindExperience = "experience"
	KindThought    = "thought"
	KindPreference = "preference"
)

var memoryKinds = map[string]bool{
	KindClaim: true, KindExperience: true, KindThought: true, KindPreference: true,
}

// Роли связи записи с сущностью (§8.3).
const (
	RoleSubject     = "subject"
	RoleParticipant = "participant"
	RolePlace       = "place"
	RoleContext     = "context"
)

var entityRoles = map[string]bool{
	RoleSubject: true, RoleParticipant: true, RolePlace: true, RoleContext: true,
}

// Item — строка memory_items.
type Item struct {
	ID           int64
	Kind         string
	Text         string
	ScopeID      string
	Status       string
	Salience     float64
	Confidence   *float64
	OccurredAt   *time.Time
	RecordedAt   time.Time
	ValidFrom    *time.Time
	ValidUntil   *time.Time
	SupersedesID *int64
	ProvenanceID int64
}

// EntityLink — связь новой записи с сущностью по имени (§8.3): сущность
// находится или создаётся в скоупе записи.
type EntityLink struct {
	Name string
	Kind string
	Role string
}

// NewItem — кандидат памяти, прошедший проверку runtime (§10.1): источник
// назначает runtime (модель ссылок не присваивает вовсе), скоуп — скоуп хода.
type NewItem struct {
	Kind         string
	Text         string
	Salience     float64
	Confidence   *float64
	OccurredAt   *time.Time
	Status       string // "" — active
	ProvenanceID int64  // разрешение, назначенное runtime (§9.1)
	Source       *SourceRef
	Links        []EntityLink
	DeriveFrom   []int64 // memory_derivations: производность новой записи
}

// Validate проверяет кандидат: типы, диапазоны, допустимые значения (§10.1).
func (n NewItem) Validate() error {
	if !memoryKinds[n.Kind] {
		return fmt.Errorf("%w: неизвестный вид записи %q", ErrInvalidCandidate, n.Kind)
	}
	if strings.TrimSpace(n.Text) == "" {
		return fmt.Errorf("%w: пустой текст записи", ErrInvalidCandidate)
	}
	if n.Salience < 0 || n.Salience > 1 {
		return fmt.Errorf("%w: salience %v вне 0..1", ErrInvalidCandidate, n.Salience)
	}
	if n.Confidence != nil && (*n.Confidence < 0 || *n.Confidence > 1) {
		return fmt.Errorf("%w: confidence %v вне 0..1", ErrInvalidCandidate, *n.Confidence)
	}
	if n.Status != "" && !IsValidMemoryStatus(n.Status) {
		return fmt.Errorf("%w: неизвестный статус %q", ErrInvalidCandidate, n.Status)
	}
	if n.ProvenanceID <= 0 {
		return fmt.Errorf("%w: не назначен provenance (разрешение назначает runtime — §9.1)", ErrInvalidCandidate)
	}
	if n.Source != nil {
		if err := n.Source.Validate(); err != nil {
			return err
		}
	}
	for _, l := range n.Links {
		if !entityRoles[l.Role] {
			return fmt.Errorf("%w: неизвестная роль связи %q", ErrInvalidCandidate, l.Role)
		}
		if !entityKinds[l.Kind] {
			return fmt.Errorf("%w: неизвестный вид сущности %q", ErrInvalidCandidate, l.Kind)
		}
		if strings.TrimSpace(l.Name) == "" {
			return fmt.Errorf("%w: пустое имя сущности в связи", ErrInvalidCandidate)
		}
	}
	return nil
}

// IsValidMemoryStatus проверяет статус записи.
func IsValidMemoryStatus(status string) bool {
	switch status {
	case "active", "disputed", "superseded", "retracted":
		return true
	}
	return false
}

// Items — репозиторий memory_items (+ memory_links, memory_sources,
// memory_derivations).
type Items struct {
	Entities EntityRepo
	Sources  SourceRepo
}

const itemCols = `id, kind, text, scope_id, status, salience, confidence,
	  occurred_at, recorded_at, valid_from, valid_until, supersedes_id, provenance_id`

// scanItem читает Item из строки, колонки в порядке itemCols.
func scanItem(row pgxRow) (Item, error) {
	var it Item
	err := row.Scan(&it.ID, &it.Kind, &it.Text, &it.ScopeID, &it.Status, &it.Salience,
		&it.Confidence, &it.OccurredAt, &it.RecordedAt, &it.ValidFrom, &it.ValidUntil,
		&it.SupersedesID, &it.ProvenanceID)
	if err != nil {
		return Item{}, err
	}
	return it, nil
}

// orTsQuery собирает tsquery с OR-семантикой из слов запроса; символы вне
// слов отбрасываются — синтаксис to_tsquery из пользовательского ввода не
// собирается. Дефис — разделитель, не часть слова (17.13): токен с дефисом
// получал во to_tsquery фразовую семантику, и «арка-фанера» не находила
// «арку из фанеры»; два токена с OR — находят оба написания. Пусто — искать
// нечего.
var orQueryWordRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

func orTsQuery(query string) string {
	words := orQueryWordRe.FindAllString(query, -1)
	if len(words) == 0 {
		return ""
	}
	return strings.Join(words, " | ")
}

// visCond — SQL-эквивалент ItemVisible (§9.2: фильтр ДО извлечения текста и
// ДО семантического ранжирования). alias — алиас memory_items в запросе,
// aud — номер параметра с аудиторией. Присоединяет provenance по p.id.
// Аудитория internal видит всё до проверки скоупа (§9.2: рефлексия работает
// отдельно, её производные наследуют ограничения при записи).
func visCond(alias string, aud int) string {
	return fmt.Sprintf(`($%[2]d = 'internal' OR (
	     (%[1]s.scope_id = $%[2]d OR %[1]s.scope_id = 'public')
	     AND p.id IS NOT NULL
	     AND NOT EXISTS (
	       SELECT 1 FROM unnest(p.scopes) AS s(el)
	       WHERE s.el NOT IN ('public', 'internal') AND s.el !~ '^chat:-?[0-9]+$')
	     AND ('public' = ANY(p.scopes) OR $%[2]d = ANY(p.scopes))))`,
		alias, aud)
}

// Insert записывает кандидата от имени runtime: item + источник + связи,
// одной транзакцией вызывающего (§10.1). Скоуп назначает runtime.
// Точный дубль активной записи (тот же скоуп, текст с точностью до регистра
// и краёв) не вставляется и не плодит строку: уникальное выражение
// (scope_id, md5(lower(btrim(text)))) WHERE status='active' + ON CONFLICT DO
// NOTHING (блок 18.8) — возврат существующей записи без ошибки, честный
// пропуск без дописывания источника и связей.
func (r Items) Insert(ctx context.Context, db DBTX, item NewItem, scopeID string) (Item, error) {
	if err := item.Validate(); err != nil {
		return Item{}, err
	}
	if !IsValidScope(scopeID) {
		return Item{}, fmt.Errorf("%w: %q", ErrInvalidScope, scopeID)
	}

	status := item.Status
	if status == "" {
		status = "active"
	}
	it, err := scanItem(db.QueryRow(ctx,
		`INSERT INTO memory_items (kind, text, scope_id, provenance_id, status, salience, confidence, occurred_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (scope_id, md5(lower(btrim(text)))) WHERE status = 'active' DO NOTHING
		 RETURNING `+itemCols,
		item.Kind, item.Text, scopeID, item.ProvenanceID, status, item.Salience,
		item.Confidence, item.OccurredAt))
	if err != nil {
		if isNoRows(err) {
			return findActiveByText(ctx, db, scopeID, item.Text)
		}
		return Item{}, fmt.Errorf("memory_items: вставка: %w", err)
	}

	if err := r.attachMeta(ctx, db, it.ID, item, scopeID); err != nil {
		return Item{}, err
	}
	return it, nil
}

// findActiveByText возвращает существующую активную запись с тем же
// нормализованным текстом (путь пропуска дубля, блок 18.8).
func findActiveByText(ctx context.Context, db DBTX, scopeID, text string) (Item, error) {
	it, err := scanItem(db.QueryRow(ctx,
		`SELECT `+itemCols+` FROM memory_items
		 WHERE scope_id = $1 AND status = 'active'
		   AND md5(lower(btrim(text))) = md5(lower(btrim($2)))
		 ORDER BY id LIMIT 1`, scopeID, text))
	if err != nil {
		return Item{}, fmt.Errorf("memory_items: существующий дубль не читается: %w", err)
	}
	return it, nil
}

// attachMeta записывает источник, связи и производность; idempotent-гварды
// (NOT EXISTS) делают повторную запись того же кандидата без дублей.
func (r Items) attachMeta(ctx context.Context, db DBTX, itemID int64, item NewItem, scopeID string) error {
	if item.Source != nil {
		sourceID, err := r.Sources.EnsureSource(ctx, db, *item.Source)
		if err != nil {
			return err
		}
		if _, err := db.Exec(ctx,
			`INSERT INTO memory_sources (memory_id, source_id)
			 SELECT $1, $2 WHERE NOT EXISTS (
			   SELECT 1 FROM memory_sources WHERE memory_id = $1 AND source_id = $2)`,
			itemID, sourceID); err != nil {
			return fmt.Errorf("memory_sources: связывание: %w", err)
		}
	}

	for _, link := range item.Links {
		entity, err := r.Entities.FindOrCreate(ctx, db, link.Kind, link.Name, scopeID)
		if err != nil {
			return fmt.Errorf("memory_links: сущность %q: %w", link.Name, err)
		}
		if _, err := db.Exec(ctx,
			`INSERT INTO memory_links (memory_id, entity_id, role)
			 SELECT $1, $2, $3 WHERE NOT EXISTS (
			   SELECT 1 FROM memory_links WHERE memory_id = $1 AND entity_id = $2 AND role = $3)`,
			itemID, entity.ID, link.Role); err != nil {
			return fmt.Errorf("memory_links: связывание: %w", err)
		}
	}

	for _, inputID := range item.DeriveFrom {
		if _, err := db.Exec(ctx,
			`INSERT INTO memory_derivations (derived_id, input_memory_id)
			 SELECT $1, $2 WHERE NOT EXISTS (
			   SELECT 1 FROM memory_derivations WHERE derived_id = $1 AND input_memory_id = $2)`,
			itemID, inputID); err != nil {
			return fmt.Errorf("memory_derivations: связывание: %w", err)
		}
	}
	return nil
}

// Supersede пересматривает запись (§8.2, §10.3): новая заменяет старую,
// старая помечается superseded (только из active; disputed остаётся
// оспоренной), текст старой физически на месте, производность зафиксирована.
// Тихая перезапись запрещена: замена чужого скоупа — ErrSupersedeScope,
// повторный пересмотр заменённой/отозванной — ErrSupersedeFork.
func (r Items) Supersede(ctx context.Context, db DBTX, oldID int64, item NewItem, scopeID string) (Item, error) {
	var oldScope, oldStatus string
	err := db.QueryRow(ctx,
		"SELECT scope_id, status FROM memory_items WHERE id = $1", oldID).
		Scan(&oldScope, &oldStatus)
	if isNoRows(err) {
		return Item{}, fmt.Errorf("supersede: запись %d не найдена: %w", oldID, ErrNotFound)
	}
	if err != nil {
		return Item{}, fmt.Errorf("supersede: чтение записи %d: %w", oldID, err)
	}
	if oldScope != scopeID {
		return Item{}, fmt.Errorf("supersede: запись %d в scope %q, пересмотр в %q: %w",
			oldID, oldScope, scopeID, ErrSupersedeScope)
	}
	if oldStatus != "active" && oldStatus != "disputed" {
		return Item{}, fmt.Errorf("supersede: запись %d уже %q: %w", oldID, oldStatus, ErrSupersedeFork)
	}

	created, err := r.Insert(ctx, db, item, scopeID)
	if err != nil {
		return Item{}, fmt.Errorf("supersede: новая запись: %w", err)
	}
	if _, err := db.Exec(ctx,
		"UPDATE memory_items SET supersedes_id = $2 WHERE id = $1", created.ID, oldID); err != nil {
		return Item{}, fmt.Errorf("supersede: пометка преемственности: %w", err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE memory_items SET status = 'superseded' WHERE id = $1 AND status = 'active'`,
		oldID); err != nil {
		return Item{}, fmt.Errorf("supersede: пометка старой записи: %w", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO memory_derivations (derived_id, input_memory_id)
		 SELECT $1, $2 WHERE NOT EXISTS (
		   SELECT 1 FROM memory_derivations WHERE derived_id = $1 AND input_memory_id = $2)`,
		created.ID, oldID); err != nil {
		return Item{}, fmt.Errorf("supersede: фиксация производности: %w", err)
	}
	return created, nil
}

// Get возвращает запись по id, если она видна аудитории (ItemVisible);
// иначе ErrNotFound — существование приватной записи не раскрывается (§9.2).
func (Items) Get(ctx context.Context, db DBTX, id int64, audience string) (Item, error) {
	if !IsValidScope(audience) {
		return Item{}, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	row := db.QueryRow(ctx,
		`SELECT `+prefixedItemCols("m")+` FROM memory_items m
		 LEFT JOIN provenance p ON p.id = m.provenance_id
		 WHERE m.id = $1 AND `+visCond("m", 2),
		id, audience)
	it, err := scanItem(row)
	if isNoRows(err) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("memory_items: чтение по id: %w", err)
	}
	return it, nil
}

// SearchFTS — полнотекстовый поиск (§10.4 п.4) по русской морфологии,
// слова запроса соединяются ИЛИ (§10.4, философия №8: websearch-AND обнулял
// выдачу от одного неподходящего термина — живой прогон эвалов 2026-10-30).
// Ранжирование: покрытие запроса (ts_rank), затем salience, recorded_at.
// Фильтр видимости в WHERE — до ранжирования.
func (Items) SearchFTS(ctx context.Context, db DBTX, audience, query string, limit int) ([]Item, error) {
	if !IsValidScope(audience) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	q := orTsQuery(query)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	rows, err := db.Query(ctx,
		`SELECT `+prefixedItemCols("m")+` FROM memory_items m
		 LEFT JOIN provenance p ON p.id = m.provenance_id
		 WHERE m.status = 'active'
		   AND `+visCond("m", 1)+`
		   AND to_tsvector('russian', m.text) @@ to_tsquery('russian', $2)
		 ORDER BY ts_rank(to_tsvector('russian', m.text), to_tsquery('russian', $2)) DESC,
		          m.salience DESC, m.recorded_at DESC
		 LIMIT $3`,
		audience, q, limit)
	if err != nil {
		return nil, fmt.Errorf("memory_items: полнотекстовый поиск: %w", err)
	}
	defer rows.Close()
	return collectItems(rows)
}

// SearchSemantic — семантический pgvector-поиск (§10.4 п.4): ближайшие по
// косинусной дистанции записи с векторами той же модели. Скоуп-фильтр и
// проверка provenance в WHERE, до упорядочивания по дистанции (§9.2: до
// semantic ranking; §18: embeddings только с обязательным scope filter).
func (Items) SearchSemantic(ctx context.Context, db DBTX, audience string, embedding []float32, model string, limit int) ([]ScoredItem, error) {
	if !IsValidScope(audience) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	if len(embedding) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	rows, err := db.Query(ctx,
		`SELECT `+prefixedItemCols("m")+`, e.embedding <=> $2::vector AS distance
		 FROM memory_items m
		 JOIN memory_embeddings e ON e.memory_id = m.id AND e.model = $3
		 LEFT JOIN provenance p ON p.id = m.provenance_id
		 WHERE m.status = 'active'
		   AND `+visCond("m", 1)+`
		 ORDER BY distance
		 LIMIT $4`,
		audience, vectorLiteral(embedding), model, limit)
	if err != nil {
		return nil, fmt.Errorf("memory_items: семантический поиск: %w", err)
	}
	defer rows.Close()

	var out []ScoredItem
	for rows.Next() {
		var s ScoredItem
		if err := rows.Scan(&s.Item.ID, &s.Item.Kind, &s.Item.Text, &s.Item.ScopeID, &s.Item.Status,
			&s.Item.Salience, &s.Item.Confidence, &s.Item.OccurredAt, &s.Item.RecordedAt,
			&s.Item.ValidFrom, &s.Item.ValidUntil, &s.Item.SupersedesID, &s.Item.ProvenanceID,
			&s.Distance); err != nil {
			return nil, fmt.Errorf("memory_items: чтение строки семантического поиска: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ItemsForEntities — записи по прямым связям с сущностями (§8.3, §10.4 п.3):
// только активные и видимые аудитории; ссылка на сущность чужого чата запись
// не раскрывает.
func (Items) ItemsForEntities(ctx context.Context, db DBTX, audience string, entityIDs []int64, limit int) ([]Item, error) {
	if !IsValidScope(audience) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	if len(entityIDs) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	rows, err := db.Query(ctx,
		`SELECT DISTINCT `+prefixedItemCols("m")+` FROM memory_items m
		 JOIN memory_links l ON l.memory_id = m.id
		 LEFT JOIN provenance p ON p.id = m.provenance_id
		 WHERE m.status = 'active'
		   AND `+visCond("m", 1)+`
		   AND l.entity_id = ANY($2)
		 ORDER BY m.salience DESC, m.recorded_at DESC, m.id DESC
		 LIMIT $3`,
		audience, entityIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("memory_items: записи по сущностям: %w", err)
	}
	defer rows.Close()
	return collectItems(rows)
}

// LatestForScope — последние активные записи назначенного скоупа (карточка
// собеседника, решение 2026-11-02): «что персонаж уже знает об этом чате».
// Фильтр приватности тот же, что у инструментов: видимость проверяется в SQL
// до выдачи текста (§9.2), поэтому запись чужого чата не попадает в карточку
// даже при совпадении имени сущности. Порядок детерминирован (время, вес, id).
func (Items) LatestForScope(ctx context.Context, db DBTX, audience, scopeID string, limit int) ([]Item, error) {
	if !IsValidScope(audience) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}
	if !IsValidScope(scopeID) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScope, scopeID)
	}
	if limit <= 0 {
		limit = 10
	}

	rows, err := db.Query(ctx,
		`SELECT `+prefixedItemCols("m")+` FROM memory_items m
		 LEFT JOIN provenance p ON p.id = m.provenance_id
		 WHERE m.status = 'active'
		   AND m.scope_id = $2
		   AND `+visCond("m", 1)+`
		 ORDER BY m.recorded_at DESC, m.salience DESC, m.id DESC
		 LIMIT $3`,
		audience, scopeID, limit)
	if err != nil {
		return nil, fmt.Errorf("memory_items: последние записи скоупа: %w", err)
	}
	defer rows.Close()
	return collectItems(rows)
}

// SupersedeChain — цепочка пересмотра от записи к корню (преемственность
// мнений, §10.3). Обход идёт по всей цепочке, выдача фильтруется по
// аудитории: скрытое звено не показывается, но не обрывает историю выше
// (§9.2 фильтрует выдачу, а не саму преемственность).
func (Items) SupersedeChain(ctx context.Context, db DBTX, id int64, audience string) ([]Item, error) {
	if !IsValidScope(audience) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidScope, audience)
	}

	rows, err := db.Query(ctx,
		`WITH RECURSIVE chain AS (
		   SELECT m.* FROM memory_items m WHERE m.id = $1
		   UNION ALL
		   SELECT prev.* FROM memory_items prev
		   JOIN chain ON prev.id = chain.supersedes_id
		 )
		 SELECT `+prefixedItemCols("m")+` FROM memory_items m
		 JOIN chain ON chain.id = m.id
		 LEFT JOIN provenance p ON p.id = m.provenance_id
		 WHERE `+visCond("m", 2)+`
		 ORDER BY m.recorded_at DESC`,
		id, audience)
	if err != nil {
		return nil, fmt.Errorf("memory_items: цепочка пересмотра: %w", err)
	}
	defer rows.Close()
	return collectItems(rows)
}

// SourceCite — цитата источника для выдачи модели (§8.3, §10.4 п.6):
// вид и стабильная ссылка; сущность источника (текст, metadata) в выдачу
// не идёт. Provenance в выдачу не попадает — это механизм разрешений, а не
// цитата (§9.1).
type SourceCite struct {
	Kind string
	Ref  string
}

// SourcesFor — источники записей одним запросом по пачке id, сгруппированные
// по записи в порядке возрастания id источника.
func (Items) SourcesFor(ctx context.Context, db DBTX, ids []int64) (map[int64][]SourceCite, error) {
	out := make(map[int64][]SourceCite, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx,
		`SELECT ms.memory_id, s.kind, s.ref
		 FROM memory_sources ms
		 JOIN sources s ON s.id = ms.source_id
		 WHERE ms.memory_id = ANY($1)
		 ORDER BY ms.memory_id, s.id`,
		ids)
	if err != nil {
		return nil, fmt.Errorf("memory_sources: выборка источников: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var memoryID int64
		var cite SourceCite
		if err := rows.Scan(&memoryID, &cite.Kind, &cite.Ref); err != nil {
			return nil, fmt.Errorf("memory_sources: чтение строки: %w", err)
		}
		out[memoryID] = append(out[memoryID], cite)
	}
	return out, rows.Err()
}

// prefixedItemCols — колонки itemCols с алиасом (для DISTINCT-запросов).
func prefixedItemCols(alias string) string {
	return alias + ".id, " + alias + ".kind, " + alias + ".text, " + alias + ".scope_id, " +
		alias + ".status, " + alias + ".salience, " + alias + ".confidence, " +
		alias + ".occurred_at, " + alias + ".recorded_at, " + alias + ".valid_from, " +
		alias + ".valid_until, " + alias + ".supersedes_id, " + alias + ".provenance_id"
}

func collectItems(rows pgxRows) ([]Item, error) {
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("memory_items: чтение строки: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
