//go:build db

// БД-тесты репозиториев памяти (этап 9.3): scope-изоляция (§9.1–§9.2),
// missing/invalid provenance → internal, supersede-цепочки (§10.3: тихая
// перезапись запрещена), stances с основаниями, полнотекстовый и
// семантический поиск с фильтром до ранжирования. Поведение — паритет
// справочника src/memory/memory-repo.db.test.ts, перенесённый на схему §18
// с обязательным provenance.
package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var testRefCounter int

func nextRef(prefix string) string {
	testRefCounter++
	return fmt.Sprintf("%s:%d", prefix, testRefCounter)
}

// seedOpts — параметры посева записи памяти.
type seedOpts struct {
	salience   float64
	provScopes []string // разрешённые аудитории provenance; nil = [scope записи]
	links      []EntityLink
	deriveFrom []int64
}

func seedItem(t *testing.T, ctx context.Context, db DBTX, scopeID, text string, opt seedOpts) Item {
	t.Helper()
	scopes := opt.provScopes
	if scopes == nil {
		scopes = []string{scopeID}
	}
	provID, err := (SourceRepo{}).EnsureProvenance(ctx, db, SourceJournalEvent, nextRef("prov"), scopes)
	if err != nil {
		t.Fatalf("provenance не создан: %v", err)
	}
	conf := 0.8
	item, err := (Items{}).Insert(ctx, db, NewItem{
		Kind:         KindClaim,
		Text:         text,
		Salience:     opt.salience,
		Confidence:   &conf,
		ProvenanceID: provID,
		Source:       &SourceRef{Kind: SourceJournalEvent, Ref: nextRef("turn")},
		Links:        opt.links,
		DeriveFrom:   opt.deriveFrom,
	}, scopeID)
	if err != nil {
		t.Fatalf("запись %q не вставлена: %v", text, err)
	}
	return item
}

// seedRawProvenance сеет provenance с произвольными (в т.ч. битыми) скоупами
// напрямую в SQL: репозиторий битые скоупы отклоняет, а чтение обязано
// устоять перед таким рядком (defense in depth §9.1).
func seedRawProvenance(t *testing.T, ctx context.Context, db DBTX, scopes []string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(ctx,
		"INSERT INTO provenance (kind, ref, scopes) VALUES ('journal_event', $1, $2) RETURNING id",
		nextRef("raw-prov"), scopes).Scan(&id); err != nil {
		t.Fatalf("raw provenance не вставлен: %v", err)
	}
	return id
}

// seedRawItem вставляет запись напрямую (обход репозитория) — для цепочек,
// которых рабочий путь supersede создать не может.
func seedRawItem(t *testing.T, ctx context.Context, db DBTX, scopeID, text, status string, provenanceID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(ctx,
		`INSERT INTO memory_items (kind, text, scope_id, status, provenance_id)
		 VALUES ('claim', $1, $2, $3, $4) RETURNING id`,
		text, scopeID, status, provenanceID).Scan(&id); err != nil {
		t.Fatalf("raw запись %q не вставлена: %v", text, err)
	}
	return id
}

// stampRecorded фиксирует recorded_at для детерминированного порядка выборок.
func stampRecorded(t *testing.T, ctx context.Context, db DBTX, id int64, at string) {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatalf("штамп %q не разобран: %v", at, err)
	}
	if _, err := db.Exec(ctx, "UPDATE memory_items SET recorded_at = $2 WHERE id = $1", id, parsed); err != nil {
		t.Fatalf("recorded_at не обновлён: %v", err)
	}
}

func texts(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Text)
	}
	return out
}

const (
	scopeA = "chat:111"
	scopeB = "chat:222"
)

func TestRepoScopeIsolation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	items := Items{}

	privateText := "Алиса держит тайный пароль от сейфа в шкатулке"
	publicText := "Публичный тайный пароль опубликован в открытом блоге"
	seedItem(t, ctx, pool, scopeA, privateText, seedOpts{salience: 0.9})
	seedItem(t, ctx, pool, ScopePublic, publicText, seedOpts{salience: 0.5})

	t.Run("полнотекст: приватная запись чата A не находится из B; public видна обоим", func(t *testing.T) {
		fromA, err := items.SearchFTS(ctx, pool, scopeA, "тайный пароль", 10)
		if err != nil {
			t.Fatalf("SearchFTS: %v", err)
		}
		fromB, err := items.SearchFTS(ctx, pool, scopeB, "тайный пароль", 10)
		if err != nil {
			t.Fatalf("SearchFTS: %v", err)
		}
		if got := texts(fromA); len(got) != 2 {
			t.Errorf("из A найдено %v, хочу обе записи", got)
		}
		if got := texts(fromB); len(got) != 1 || got[0] != publicText {
			t.Errorf("из B найдено %v, хочу только public", got)
		}
	})

	t.Run("internal-запись не видна ни одному чату", func(t *testing.T) {
		seedItem(t, ctx, pool, ScopeInternal, "Внутреннее размышление про тайный пароль", seedOpts{})
		for _, aud := range []string{scopeA, scopeB} {
			found, err := items.SearchFTS(ctx, pool, aud, "тайный пароль", 10)
			if err != nil {
				t.Fatalf("SearchFTS(%s): %v", aud, err)
			}
			for _, it := range found {
				if it.ScopeID == ScopeInternal {
					t.Errorf("internal-запись утекла в %s", aud)
				}
			}
		}
		// внутреннему контексту — видна
		internal, err := items.SearchFTS(ctx, pool, ScopeInternal, "тайный пароль", 10)
		if err != nil {
			t.Fatalf("SearchFTS(internal): %v", err)
		}
		if len(internal) == 0 {
			t.Error("internal-аудитория не видит internal-запись")
		}
	})

	t.Run("по сущности: ссылка на сущность чужого чата запись не раскрывает", func(t *testing.T) {
		mete := seedItem(t, ctx, pool, scopeA, "Секретный проект метеор запускается в марте", seedOpts{
			links: []EntityLink{{Name: "Метеор", Kind: EntityProject, Role: RoleSubject}},
		})
		var entityID int64
		if err := pool.QueryRow(ctx,
			"SELECT id FROM entities WHERE lower(canonical_name) = 'метеор'").Scan(&entityID); err != nil {
			t.Fatalf("сущность не найдена: %v", err)
		}
		fromB, err := items.ItemsForEntities(ctx, pool, scopeB, []int64{entityID}, 10)
		if err != nil {
			t.Fatalf("ItemsForEntities(B): %v", err)
		}
		if len(fromB) != 0 {
			t.Errorf("чужой чат видит %v", texts(fromB))
		}
		fromA, err := items.ItemsForEntities(ctx, pool, scopeA, []int64{entityID}, 10)
		if err != nil {
			t.Fatalf("ItemsForEntities(A): %v", err)
		}
		if len(fromA) != 1 || fromA[0].ID != mete.ID {
			t.Errorf("свой чат видит %v, хочу только свою запись", texts(fromA))
		}
	})
}

func TestRepoProvenanceGovernsVisibility(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	items := Items{}

	t.Run("пустое разрешение (пересечение двух приватных аудиторий) → internal", func(t *testing.T) {
		// Рабочий путь консолидации: derived scope internal, provenance с
		// пустым списком аудиторий.
		seedItem(t, ctx, pool, ScopeInternal, "Сводка без общего чата", seedOpts{provScopes: []string{}})
		found, err := items.SearchFTS(ctx, pool, scopeA, "Сводка", 10)
		if err != nil {
			t.Fatalf("SearchFTS: %v", err)
		}
		if len(found) != 0 {
			t.Errorf("запись без разрешённых аудиторий утекла в %s: %v", scopeA, texts(found))
		}
		internal, err := items.SearchFTS(ctx, pool, ScopeInternal, "Сводка", 10)
		if err != nil {
			t.Fatalf("SearchFTS(internal): %v", err)
		}
		if len(internal) != 1 {
			t.Errorf("internal-аудитория видит %d, хочу 1", len(internal))
		}
	})

	t.Run("invalid provenance → internal: битые скоупы чату не выдаются (§9.1)", func(t *testing.T) {
		prov := seedRawProvenance(t, ctx, pool, []string{"chat:abc"})
		seedRawItem(t, ctx, pool, scopeA, "Запись с битым разрешением", "active", prov)

		found, err := items.SearchFTS(ctx, pool, scopeA, "битым разрешением", 10)
		if err != nil {
			t.Fatalf("SearchFTS: %v", err)
		}
		if len(found) != 0 {
			t.Errorf("запись с битым provenance утекла в %s: %v", scopeA, texts(found))
		}
		internal, err := items.SearchFTS(ctx, pool, ScopeInternal, "битым разрешением", 10)
		if err != nil {
			t.Fatalf("SearchFTS(internal): %v", err)
		}
		if len(internal) != 1 {
			t.Errorf("битое provenance должно вести себя как internal, internal видит %d", len(internal))
		}
	})

	t.Run(" Get: невидимая запись неотличима от несуществующей", func(t *testing.T) {
		item := seedItem(t, ctx, pool, scopeA, "Приватный факт про памир", seedOpts{})
		if _, err := items.Get(ctx, pool, item.ID, scopeB); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get из чужого чата: err = %v, хочу ErrNotFound", err)
		}
		got, err := items.Get(ctx, pool, item.ID, scopeA)
		if err != nil || got.ID != item.ID {
			t.Errorf("Get своим чатом: %v, %v", got, err)
		}
		if _, err := items.Get(ctx, pool, 987654, scopeA); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get несуществующей: err = %v, хочу ErrNotFound", err)
		}
	})

	t.Run("валидированное расширение: provenance public выдаёт public-запись чужому чату", func(t *testing.T) {
		seedItem(t, ctx, pool, ScopePublic, "Опубликованный факт про ибис", seedOpts{provScopes: []string{ScopePublic}})
		found, err := items.SearchFTS(ctx, pool, scopeB, "ибис", 10)
		if err != nil {
			t.Fatalf("SearchFTS: %v", err)
		}
		if len(found) != 1 {
			t.Errorf("public-запись не видна чужому чату: %v", texts(found))
		}
	})

	t.Run("provenance сужает: public-запись с chat-only разрешением чужому чату не видна", func(t *testing.T) {
		// Битая проводка: scope_id public, но разрешение только chat:A.
		// Чтение обязано держать более строгое из двух (§9.1: расширение —
		// только валидированной операцией).
		prov, err := (SourceRepo{}).EnsureProvenance(ctx, pool, SourceJournalEvent, nextRef("prov"), []string{scopeA})
		if err != nil {
			t.Fatalf("provenance: %v", err)
		}
		id := seedRawItem(t, ctx, pool, ScopePublic, "Смешанный скоуп и разрешение", "active", prov)
		stampRecorded(t, ctx, pool, id, "2026-10-30T10:00:00Z")

		fromA, err := items.SearchFTS(ctx, pool, scopeA, "Смешанный скоуп", 10)
		if err != nil {
			t.Fatalf("SearchFTS(A): %v", err)
		}
		if len(fromA) != 1 {
			t.Errorf("свой чат должен видеть запись: %v", texts(fromA))
		}
		fromB, err := items.SearchFTS(ctx, pool, scopeB, "Смешанный скоуп", 10)
		if err != nil {
			t.Fatalf("SearchFTS(B): %v", err)
		}
		if len(fromB) != 0 {
			t.Errorf("разрешение provenance обязано сузить выдачу: %v", texts(fromB))
		}
	})
}

func TestRepoEntitiesAndAliases(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	entities := EntityRepo{}

	t.Run("сущность чата A не находится из B; своя и public — находятся", func(t *testing.T) {
		seedItem(t, ctx, pool, scopeA, "Секретный проект метеор запускается", seedOpts{
			links: []EntityLink{{Name: "Метеор", Kind: EntityProject, Role: RoleSubject}},
		})
		seedRawEntity(t, ctx, pool, EntityTopic, "Новый год", ScopePublic)

		found, err := entities.ResolveByName(ctx, pool, "метеор", scopeA)
		if err != nil || len(found) != 1 {
			t.Fatalf("ResolveByName(метеор, A) = %v, %v", found, err)
		}
		foreign, err := entities.ResolveByName(ctx, pool, "метеор", scopeB)
		if err != nil || len(foreign) != 0 {
			t.Errorf("чужой чат нашёл %v", foreign)
		}
		public, err := entities.ResolveByName(ctx, pool, "НОВЫЙ ГОД", scopeB)
		if err != nil || len(public) != 1 {
			t.Errorf("public-сущность по другому регистру: %v, %v", public, err)
		}
	})

	t.Run("алиас чужого скоупа не открывает даже публичную сущность (§8.1)", func(t *testing.T) {
		entity := seedRawEntity(t, ctx, pool, EntityTopic, "Праздник урожая", ScopePublic)
		// Алиас назначен только чату A.
		if _, err := pool.Exec(ctx,
			"INSERT INTO entity_aliases (entity_id, alias, scope_id) VALUES ($1, 'осенний праздник', $2)",
			entity, scopeA); err != nil {
			t.Fatalf("алиас не вставлен: %v", err)
		}
		fromA, err := entities.ResolveByName(ctx, pool, "осенний праздник", scopeA)
		if err != nil || len(fromA) != 1 {
			t.Errorf("свой чат не нашёл по алиасу: %v, %v", fromA, err)
		}
		fromB, err := entities.ResolveByName(ctx, pool, "осенний праздник", scopeB)
		if err != nil || len(fromB) != 0 {
			t.Errorf("чужой чат нашёл по чужому алиасу: %v", fromB)
		}
		// но по каноническому имени публичная сущность видна обоим
		canonical, err := entities.ResolveByName(ctx, pool, "праздник урожая", scopeB)
		if err != nil || len(canonical) != 1 {
			t.Errorf("public-сущность по имени: %v, %v", canonical, err)
		}
	})

	t.Run("AddAlias наследует скоуп сущности и идемпотентен", func(t *testing.T) {
		item := seedItem(t, ctx, pool, scopeA, "Метеор стартует в марте", seedOpts{
			links: []EntityLink{{Name: "Метеор-два", Kind: EntityProject, Role: RoleSubject}},
		})
		var entityID int64
		if err := pool.QueryRow(ctx, "SELECT entity_id FROM memory_links WHERE memory_id = $1", item.ID).Scan(&entityID); err != nil {
			t.Fatalf("связь не найдена: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := entities.AddAlias(ctx, pool, entityID, "проект-икс"); err != nil {
				t.Fatalf("AddAlias: %v", err)
			}
		}
		var scopeID string
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT scope_id, count(*) FROM entity_aliases WHERE alias = 'проект-икс' GROUP BY scope_id`).
			Scan(&scopeID, &count); err != nil {
			t.Fatalf("алиас не найден: %v", err)
		}
		if scopeID != scopeA {
			t.Errorf("алиас в скоупе %q, хочу %q", scopeID, scopeA)
		}
		if count != 1 {
			t.Errorf("алиасов = %d, хочу 1 (идемпотентность)", count)
		}
	})

	t.Run("одно имя в двух чатах — две разные сущности (§8.5)", func(t *testing.T) {
		seedItem(t, ctx, pool, scopeA, "Горы любимы", seedOpts{
			links: []EntityLink{{Name: "Горы", Kind: EntityTopic, Role: RoleSubject}},
		})
		seedItem(t, ctx, pool, scopeB, "Горы обсуждены", seedOpts{
			links: []EntityLink{{Name: "Горы", Kind: EntityTopic, Role: RoleSubject}},
		})
		rows, err := pool.Query(ctx, "SELECT scope_id FROM entities WHERE lower(canonical_name) = 'горы'")
		if err != nil {
			t.Fatalf("выборка сущностей: %v", err)
		}
		defer rows.Close()
		scopes := map[string]int{}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("чтение: %v", err)
			}
			scopes[s]++
		}
		if len(scopes) != 2 || scopes[scopeA] != 1 || scopes[scopeB] != 1 {
			t.Errorf("сущности Горы: %v, хочу по одной на чат", scopes)
		}
	})
}

func seedRawEntity(t *testing.T, ctx context.Context, db DBTX, kind, name, scopeID string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(ctx,
		"INSERT INTO entities (kind, canonical_name, scope_id) VALUES ($1, $2, $3) RETURNING id",
		kind, name, scopeID).Scan(&id); err != nil {
		t.Fatalf("сущность %q не вставлена: %v", name, err)
	}
	return id
}

func TestRepoSupersede(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	items := Items{}

	// Вставки пересмотра — в транзакции, как будет делать runtime (§10.1).
	insert := func(scopeID, text string) Item {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("tx: %v", err)
		}
		defer tx.Rollback(ctx)
		item := seedItem(t, ctx, tx, scopeID, text, seedOpts{})
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return item
	}
	supersede := func(oldID int64, scopeID, text string) Item {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("tx: %v", err)
		}
		defer tx.Rollback(ctx)
		provID, err := (SourceRepo{}).EnsureProvenance(ctx, tx, SourceJournalEvent, nextRef("prov"), []string{scopeID})
		if err != nil {
			t.Fatalf("provenance: %v", err)
		}
		created, err := items.Supersede(ctx, tx, oldID, NewItem{
			Kind: KindClaim, Text: text, Salience: 0.6, ProvenanceID: provID,
			Source: &SourceRef{Kind: SourceJournalEvent, Ref: nextRef("turn")},
		}, scopeID)
		if err != nil {
			t.Fatalf("Supersede: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return created
	}

	t.Run("пересмотр: старая superseded, текст на месте, производность и источник зафиксированы", func(t *testing.T) {
		old := insert(scopeA, "Пользователь живёт в Москве")
		updated := supersede(old.ID, scopeA, "Пользователь живёт в Питере")

		var oldStatus, oldScope, oldText string
		if err := pool.QueryRow(ctx,
			"SELECT status, scope_id, text FROM memory_items WHERE id = $1", old.ID).
			Scan(&oldStatus, &oldScope, &oldText); err != nil {
			t.Fatalf("старая запись не читается: %v", err)
		}
		if oldStatus != "superseded" || oldScope != scopeA || oldText != "Пользователь живёт в Москве" {
			t.Errorf("старая запись: status=%s scope=%s text=%q", oldStatus, oldScope, oldText)
		}
		var supersedes *int64
		if err := pool.QueryRow(ctx,
			"SELECT supersedes_id FROM memory_items WHERE id = $1", updated.ID).Scan(&supersedes); err != nil {
			t.Fatalf("новая запись не читается: %v", err)
		}
		if supersedes == nil || *supersedes != old.ID {
			t.Errorf("supersedes_id = %v, хочу %d", supersedes, old.ID)
		}
		var derivations int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM memory_derivations WHERE derived_id = $1 AND input_memory_id = $2",
			updated.ID, old.ID).Scan(&derivations); err != nil {
			t.Fatalf("производность не читается: %v", err)
		}
		if derivations != 1 {
			t.Errorf("строк производности = %d, хочу 1", derivations)
		}
		// новая запись со своим runtime-источником (§10.3: отдельный источник)
		var sources int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM memory_sources WHERE memory_id = $1`, updated.ID).Scan(&sources); err != nil {
			t.Fatalf("источники не читаются: %v", err)
		}
		if sources != 1 {
			t.Errorf("источников новой записи = %d, хочу 1", sources)
		}
	})

	t.Run("цепочка A ← B ← C идёт к корню; superseded не выдаётся поиском", func(t *testing.T) {
		a := insert(scopeA, "верю в Москву")
		b := supersede(a.ID, scopeA, "верю в Питере")
		c := supersede(b.ID, scopeA, "верю в Казани")
		stampRecorded(t, ctx, pool, a.ID, "2026-10-30T08:00:00Z")
		stampRecorded(t, ctx, pool, b.ID, "2026-10-30T09:00:00Z")
		stampRecorded(t, ctx, pool, c.ID, "2026-10-30T10:00:00Z")

		chain, err := items.SupersedeChain(ctx, pool, c.ID, scopeA)
		if err != nil {
			t.Fatalf("SupersedeChain: %v", err)
		}
		if got := texts(chain); len(got) != 3 || got[0] != "верю в Казани" || got[2] != "верю в Москву" {
			t.Errorf("цепочка = %v, хочу [Казани Питере Москве]", got)
		}
		midChain, err := items.SupersedeChain(ctx, pool, b.ID, scopeA)
		if err != nil {
			t.Fatalf("SupersedeChain(B): %v", err)
		}
		if len(midChain) != 2 {
			t.Errorf("цепочка от середины = %d звеньев, хочу 2", len(midChain))
		}

		active, err := items.SearchFTS(ctx, pool, scopeA, "верю", 10)
		if err != nil {
			t.Fatalf("SearchFTS: %v", err)
		}
		if len(active) != 1 || active[0].ID != c.ID {
			t.Errorf("поиск после пересмотра = %v, хочу только последнюю версию", texts(active))
		}
	})

	t.Run("цепочка с чужим звеном: каждое звено фильтруется по аудитории (§9.2)", func(t *testing.T) {
		// Сквозное звено чужого скоупа рабочий путь создать не может (гвард
		// выше) — сеется напрямую, как обслуживающая операция.
		root := insert(ScopePublic, "публичная версия мнения")
		foreignProv := seedRawProvenance(t, ctx, pool, []string{scopeB})
		foreign := seedRawItem(t, ctx, pool, scopeB, "чужая приватная версия", "superseded", foreignProv)
		leaf := insert(scopeA, "своя свежая версия")
		// направление преемственности: новая запись хранит id заменённой
		// (leaf.supersedes_id = foreign, foreign.supersedes_id = root)
		if _, err := pool.Exec(ctx, "UPDATE memory_items SET supersedes_id = $2 WHERE id = $1", foreign, root.ID); err != nil {
			t.Fatalf("связка foreign→root: %v", err)
		}
		if _, err := pool.Exec(ctx, "UPDATE memory_items SET supersedes_id = $2 WHERE id = $1", leaf.ID, foreign); err != nil {
			t.Fatalf("связка leaf→foreign: %v", err)
		}
		stampRecorded(t, ctx, pool, root.ID, "2026-10-30T10:00:00Z")
		stampRecorded(t, ctx, pool, foreign, "2026-10-30T11:00:00Z")
		stampRecorded(t, ctx, pool, leaf.ID, "2026-10-30T12:00:00Z")

		fromA, err := items.SupersedeChain(ctx, pool, leaf.ID, scopeA)
		if err != nil {
			t.Fatalf("SupersedeChain(A): %v", err)
		}
		if got := texts(fromA); len(got) != 2 || got[0] != "своя свежая версия" || got[1] != "публичная версия мнения" {
			t.Errorf("цепочка из A = %v, чужое звено обязано отсекаться", got)
		}
		fromB, err := items.SupersedeChain(ctx, pool, leaf.ID, scopeB)
		if err != nil {
			t.Fatalf("SupersedeChain(B): %v", err)
		}
		if got := texts(fromB); len(got) != 2 || got[0] != "чужая приватная версия" || got[1] != "публичная версия мнения" {
			t.Errorf("цепочка из B = %v, чужой лист обязан отсекаться", got)
		}
	})

	t.Run("смена scope отклоняется; повторный пересмотр отклоняется (§10.3)", func(t *testing.T) {
		a := insert(scopeA, "приватное мнение чата A")

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("tx: %v", err)
		}
		provID, err := (SourceRepo{}).EnsureProvenance(ctx, tx, SourceJournalEvent, nextRef("prov"), []string{scopeB})
		if err != nil {
			t.Fatalf("provenance: %v", err)
		}
		_, err = items.Supersede(ctx, tx, a.ID, NewItem{
			Kind: KindClaim, Text: "попытка легализовать в чате B", Salience: 0.5,
			ProvenanceID: provID, Source: &SourceRef{Kind: SourceJournalEvent, Ref: nextRef("turn")},
		}, scopeB)
		if !errors.Is(err, ErrSupersedeScope) {
			t.Errorf("пересмотр в чужой скоуп: err = %v, хочу ErrSupersedeScope", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}

		b := supersede(a.ID, scopeA, "живу в Питере")
		tx2, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("tx2: %v", err)
		}
		provID2, err := (SourceRepo{}).EnsureProvenance(ctx, tx2, SourceJournalEvent, nextRef("prov"), []string{scopeA})
		if err != nil {
			t.Fatalf("provenance: %v", err)
		}
		_, err = items.Supersede(ctx, tx2, a.ID, NewItem{
			Kind: KindClaim, Text: "живу в Казани (форк)", Salience: 0.5,
			ProvenanceID: provID2, Source: &SourceRef{Kind: SourceJournalEvent, Ref: nextRef("turn")},
		}, scopeA)
		if !errors.Is(err, ErrSupersedeFork) {
			t.Errorf("повторный пересмотр: err = %v, хочу ErrSupersedeFork", err)
		}
		if err := tx2.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}

		var statusA, statusB string
		if err := pool.QueryRow(ctx, "SELECT status FROM memory_items WHERE id = $1", a.ID).Scan(&statusA); err != nil {
			t.Fatalf("чтение A: %v", err)
		}
		if err := pool.QueryRow(ctx, "SELECT status FROM memory_items WHERE id = $1", b.ID).Scan(&statusB); err != nil {
			t.Fatalf("чтение B: %v", err)
		}
		if statusA != "superseded" || statusB != "active" {
			t.Errorf("после отказов: A=%s B=%s, хочу superseded/active", statusA, statusB)
		}
	})

	t.Run("ensureSource и EnsureProvenance идемпотентны", func(t *testing.T) {
		sources := SourceRepo{}
		ref := nextRef("idem")
		first, err := sources.EnsureSource(ctx, pool, SourceRef{Kind: SourceJournalEvent, Ref: ref})
		if err != nil {
			t.Fatalf("EnsureSource: %v", err)
		}
		second, err := sources.EnsureSource(ctx, pool, SourceRef{Kind: SourceJournalEvent, Ref: ref})
		if err != nil {
			t.Fatalf("EnsureSource(2): %v", err)
		}
		if first != second {
			t.Errorf("EnsureSource вернул разные id: %d и %d", first, second)
		}
		provRef := nextRef("idem-prov")
		p1, err := sources.EnsureProvenance(ctx, pool, SourceJournalEvent, provRef, []string{scopeA})
		if err != nil {
			t.Fatalf("EnsureProvenance: %v", err)
		}
		p2, err := sources.EnsureProvenance(ctx, pool, SourceJournalEvent, provRef, []string{scopeA})
		if err != nil {
			t.Fatalf("EnsureProvenance(2): %v", err)
		}
		if p1 != p2 {
			t.Errorf("EnsureProvenance вернул разные id: %d и %d", p1, p2)
		}
		if _, err := sources.EnsureProvenance(ctx, pool, SourceJournalEvent, nextRef("bad-prov"), []string{"мусор"}); !errors.Is(err, ErrInvalidScope) {
			t.Errorf("битый скоуп provenance: err = %v, хочу ErrInvalidScope", err)
		}
	})

	t.Run("кандидат без provenance отклоняется (разрешение назначает runtime — §9.1)", func(t *testing.T) {
		_, err := items.Insert(ctx, pool, NewItem{Kind: KindClaim, Text: "текст", Salience: 0.5}, scopeA)
		if !errors.Is(err, ErrInvalidCandidate) {
			t.Errorf("Insert без provenance: err = %v, хочу ErrInvalidCandidate", err)
		}
	})

	t.Run("FK не пускает несуществующий provenance", func(t *testing.T) {
		_, err := items.Insert(ctx, pool, NewItem{
			Kind: KindClaim, Text: "текст", Salience: 0.5, ProvenanceID: 987654,
		}, scopeA)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Errorf("Insert с чужим provenance: err = %v, хочу FK 23503", err)
		}
	})
}
