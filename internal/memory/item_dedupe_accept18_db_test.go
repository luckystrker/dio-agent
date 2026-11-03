//go:build db

// Дополнительные БД-проверки приёмки блока 18.8 (тестировщик): сценарии, не
// покрытые тестами разработчика, — уникальный индекс как барьер на уровне БД
// (для любого пишущего, не только Items.Insert) и миграция 000014 на грязных
// данных (существующие дубли): реальный путь обновления прод-базы.
package memory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/migrations"
)

// TestActiveDedupeIndexBlocksRawInserts — барьер живёт в БД (частичный
// уникальный индекс), а не только в ON CONFLICT пути Items.Insert: прямой
// SQL-вставкa активного дубля отклоняется; не-active статус тем же текстом
// проходит (индекс покрывает только active, блок 18.8).
func TestActiveDedupeIndexBlocksRawInserts(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	scope := "chat:741"
	provID, err := (SourceRepo{}).EnsureProvenance(ctx, pool, SourceJournalEvent, nextRef("barrier"), []string{scope})
	if err != nil {
		t.Fatalf("provenance не создан: %v", err)
	}
	if _, err := (Items{}).Insert(ctx, pool, NewItem{
		Kind: KindClaim, Text: "Коля ездит на дачу по субботам", Salience: 0.6, ProvenanceID: provID,
	}, scope); err != nil {
		t.Fatalf("первая вставка обязана проходить: %v", err)
	}

	// Прямая вставка активного дубля (другой регистр, края) — нарушение
	// уникальности, независимо от пути записи.
	_, err = pool.Exec(ctx,
		`INSERT INTO memory_items (kind, text, scope_id, provenance_id, salience)
		 VALUES ('claim', '  КОЛЯ ездит на дачу по субботам ', $1, $2, 0.5)`, scope, provID)
	var pgErr *pgconn.PgError
	if err == nil || !(errors.As(err, &pgErr) && pgErr.Code == "23505") {
		t.Fatalf("прямая вставка активного дубля обязана нарушать уникальный индекс (блок 18.8), ошибка: %v", err)
	}

	// Тот же текст в не-active статусе — индекс не покрывает, проходит.
	if _, err := pool.Exec(ctx,
		`INSERT INTO memory_items (kind, text, scope_id, provenance_id, status, salience)
		 VALUES ('claim', 'Коля ездит на дачу по субботам', $1, $2, 'retracted', 0.5)`, scope, provID); err != nil {
		t.Fatalf("не-active запись тем же текстом обязана проходить (индекс только active): %v", err)
	}
}

// newPoolWithoutDedupe — свежая база со схемой ДО миграции 000014: миграции
// применяются все, кроме дедупликационной (реальная прод-база до обновления).
func newPoolWithoutDedupe(t *testing.T) *runtime.Pool {
	t.Helper()
	ctx := context.Background()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL недоступен — БД-тест пропущен: %v", err)
	}
	defer admin.Close(ctx)

	dbName := fmt.Sprintf("dio_memory_dedupe_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("тестовая база не создана: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)")
	})

	subset := fstest.MapFS{}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("миграции не читаются: %v", err)
	}
	for _, name := range names {
		if name == "000014_memory_active_dedupe.sql" {
			continue
		}
		raw, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("миграция %s не читается: %v", name, err)
		}
		subset[name] = &fstest.MapFile{Data: raw}
	}

	testDSN, err := swapDatabaseInDSN(dsn, dbName)
	if err != nil {
		t.Fatalf("DSN не разобран: %v", err)
	}
	pool, err := runtime.NewPool(ctx, testDSN)
	if err != nil {
		t.Fatalf("пул не создан: %v", err)
	}
	t.Cleanup(pool.Close)

	result, err := runtime.Migrate(ctx, pool, subset)
	if err != nil {
		t.Fatalf("миграции без 000014 не применены: %v", err)
	}
	// Набор без одной миграции 000014: все остальные (число файлов минус один),
	// включая 000015 (стикеры), 000016 (скука), 000017/000018 (этап 29), 000019 (объединение сущностей).
	wantApplied := len(names) - 1
	if result.Applied != wantApplied || result.Skipped != 0 {
		t.Fatalf("миграции: applied=%d skipped=%d, хочу %d/0", result.Applied, result.Skipped, wantApplied)
	}
	return pool
}

// TestDedupeMigrationCollapsesExistingDuplicates — миграция 000014 обязана
// применяться на базе с уже накопленными точными дублями (иначе обновление
// прода падает на создании индекса): остаются старейшие строки, связи и
// источники младших вывозятся, индекс начинает работать.
func TestDedupeMigrationCollapsesExistingDuplicates(t *testing.T) {
	ctx := context.Background()
	pool := newPoolWithoutDedupe(t)

	scope := "chat:742"
	provID, err := (SourceRepo{}).EnsureProvenance(ctx, pool, SourceJournalEvent, nextRef("mig"), []string{scope})
	if err != nil {
		t.Fatalf("provenance не создан: %v", err)
	}
	entity, err := (EntityRepo{}).FindOrCreate(ctx, pool, "person", "Коля", scope)
	if err != nil {
		t.Fatalf("сущность не создана: %v", err)
	}

	// Два существующих дубля (регистр/края отличаются — нормализация та же)
	// и один не-дубль для контроля.
	var keepID, doomedID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO memory_items (kind, text, scope_id, provenance_id, salience)
		 VALUES ('claim', 'Поехал с женой делать ремонт', $1, $2, 0.6) RETURNING id`,
		scope, provID).Scan(&keepID); err != nil {
		t.Fatalf("старейший дубль не вставлен: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO memory_items (kind, text, scope_id, provenance_id, salience)
		 VALUES ('claim', ' поехал с женой делать ремонт ', $1, $2, 0.4) RETURNING id`,
		scope, provID).Scan(&doomedID); err != nil {
		t.Fatalf("младший дубль не вставлен: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memory_items (kind, text, scope_id, provenance_id, salience)
		 VALUES ('claim', 'Поехал с женой выбирать обои', $1, $2, 0.5)`, scope, provID); err != nil {
		t.Fatalf("не-дубль не вставлен: %v", err)
	}
	// Связи обеих строк: у выжившей остаются, у вывозимой уходят вместе с ней.
	for _, id := range []int64{keepID, doomedID} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO memory_links (memory_id, entity_id, role) VALUES ($1, $2, 'subject')`,
			id, entity.ID); err != nil {
			t.Fatalf("связь строки %d не вставлена: %v", id, err)
		}
	}
	// Ссылки, висящие ТОЛЬКО на младшем дубле (решение владельца 2026-11-02):
	// перед удалением переносятся на старейший — удаление дубля не теряет
	// источники и связи, которых у старейшего ещё нет.
	onlyDoomedEntity, err := (EntityRepo{}).FindOrCreate(ctx, pool, "person", "Жена", scope)
	if err != nil {
		t.Fatalf("сущность-участник не создана: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memory_links (memory_id, entity_id, role) VALUES ($1, $2, 'participant')`,
		doomedID, onlyDoomedEntity.ID); err != nil {
		t.Fatalf("связь только младшего не вставлена: %v", err)
	}
	onlyDoomedSource, err := (SourceRepo{}).EnsureSource(ctx, pool, SourceRef{
		Kind: SourceJournalEvent, Ref: nextRef("only-doomed"),
	})
	if err != nil {
		t.Fatalf("источник только младшего не создан: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memory_sources (memory_id, source_id) VALUES ($1, $2)`,
		doomedID, onlyDoomedSource); err != nil {
		t.Fatalf("источник только младшего не связан: %v", err)
	}

	// До миграции дубли живут рядом.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_items WHERE scope_id = $1 AND status = 'active'`, scope).Scan(&n); err != nil {
		t.Fatalf("активные записи не читаются: %v", err)
	}
	if n != 3 {
		t.Fatalf("посев: активных строк %d, хочу 3", n)
	}

	// Реальный путь обновления: полный набор миграций поверх грязной базы.
	result, err := runtime.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatalf("миграция 000014 обязана применяться поверх существующих дублей: %v", err)
	}
	if result.Applied != 1 || result.Skipped != len(migrationNames(t))-1 {
		t.Fatalf("миграции: applied=%d skipped=%d, хочу 1/%d", result.Applied, result.Skipped, len(migrationNames(t))-1)
	}

	// Схлопнуто до старейшей строки; её исходная связь цела, ссылка, висевшая
	// только на младшем, перенесена на старейший (решение владельца 2026-11-02).
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_items WHERE scope_id = $1 AND status = 'active'`, scope).Scan(&n); err != nil {
		t.Fatalf("активные записи не читаются: %v", err)
	}
	if n != 2 {
		t.Fatalf("после миграции активных строк %d, хочу 2 (дубль схлопнут, не-дубль жив)", n)
	}
	var links int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_links WHERE memory_id = $1`, keepID).Scan(&links); err != nil {
		t.Fatalf("связи выжившей строки не читаются: %v", err)
	}
	if links != 2 {
		t.Fatalf("у старейшей обязаны остаться своя связь и прийти перенос с младшего, связей %d, хочу 2", links)
	}
	var hasKept, hasMoved bool
	if err := pool.QueryRow(ctx,
		`SELECT bool_or(role = 'subject' AND entity_id = $2), bool_or(role = 'participant' AND entity_id = $3)
		 FROM memory_links WHERE memory_id = $1`, keepID, entity.ID, onlyDoomedEntity.ID).
		Scan(&hasKept, &hasMoved); err != nil {
		t.Fatalf("связи выжившей строки не читаются: %v", err)
	}
	if !hasKept {
		t.Fatal("исходная связь старейшей строки обязана оставаться")
	}
	if !hasMoved {
		t.Fatal("связь, висевшая только на младшем дубле, обязана переноситься на старейший (решение 2026-11-02)")
	}
	var movedSource bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM memory_sources WHERE memory_id = $1 AND source_id = $2)`,
		keepID, onlyDoomedSource).Scan(&movedSource); err != nil {
		t.Fatalf("источники выжившей строки не читаются: %v", err)
	}
	if !movedSource {
		t.Fatal("источник, висевший только на младшем дубле, обязан переноситься на старейший (решение 2026-11-02)")
	}

	// Индекс работает: повторная вставка того же текста через Items.Insert
	// возвращает выжившую строку без ошибки.
	it, err := (Items{}).Insert(ctx, pool, NewItem{
		Kind: KindClaim, Text: "ПОЕХАЛ С ЖЕНОЙ ДЕЛАТЬ РЕМОНТ", Salience: 0.9, ProvenanceID: provID,
	}, scope)
	if err != nil {
		t.Fatalf("вставка после миграции обязана проходить: %v", err)
	}
	if it.ID != keepID {
		t.Fatalf("дубль обязан возвращать старейшую строку: got id=%d, хочу %d", it.ID, keepID)
	}
}

// TestDedupeMigrationTransfersDerivationInputs — рёбра производности при
// схлопывании дублей. Реализованный перенос (derived-сторона: обречённый дубль —
// производная запись) обязан перепривязываться к старейшей вместе со входом.
// Input-сторона (чужая производная сослалась на дубль как на вход) сегодня
// вывозится без переноса — прод-писателей memory_derivations нет (DeriveFrom
// никем не передаётся, Supersede — мёртвый код, ТЗ 18.8), кейс недостижим на
// реальных данных; поведение зафиксировано как есть и пересматривается вместе
// с появлением писателя derivations (бэклог: решение владельца 2026-11-02
// «удаление дубля не теряет ссылки» тогда распространяется и на input-сторону).
func TestDedupeMigrationTransfersDerivationInputs(t *testing.T) {
	ctx := context.Background()
	pool := newPoolWithoutDedupe(t)

	scope := "chat:743"
	provID, err := (SourceRepo{}).EnsureProvenance(ctx, pool, SourceJournalEvent, nextRef("deriv"), []string{scope})
	if err != nil {
		t.Fatalf("provenance не создан: %v", err)
	}

	seed := func(text string) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO memory_items (kind, text, scope_id, provenance_id, salience)
			 VALUES ('claim', $1, $2, $3, 0.5) RETURNING id`,
			text, scope, provID).Scan(&id); err != nil {
			t.Fatalf("запись %q не вставлена: %v", text, err)
		}
		return id
	}
	keepID := seed("Коля ездит на дачу по субботам")
	doomedID := seed("коля ездит на дачу по субботам") // точный дубль (регистр)
	inputID := seed("Коля возит рассаду весной")       // вход производной записи-дубля
	derivedID := seed("Коля — дачник выходного дня")   // производная, сослалась на дубль

	edge := func(derived, input int64) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO memory_derivations (derived_id, input_memory_id) VALUES ($1, $2)`,
			derived, input); err != nil {
			t.Fatalf("ребро производности (%d ← %d) не вставлено: %v", derived, input, err)
		}
	}
	edge(doomedID, inputID)   // derived-сторона: дубль — производная запись
	edge(derivedID, doomedID) // input-сторона: дубль — вход чужой производной

	if _, err := runtime.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("миграция 000014 обязана применяться: %v", err)
	}

	exists := func(derived, input int64) bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM memory_derivations
			 WHERE derived_id = $1 AND input_memory_id = $2)`,
			derived, input).Scan(&ok); err != nil {
			t.Fatalf("рёбра производности не читаются: %v", err)
		}
		return ok
	}

	// Derived-сторона: ребро производной записи-дубля перенесено на старейшую.
	if !exists(keepID, inputID) {
		t.Fatal("ребро производной записи-дубля обязано переноситься на старейший дубль (решение 2026-11-02)")
	}
	if exists(doomedID, inputID) {
		t.Fatal("ребро на удалённом дубле невозможно (FK)")
	}

	// Input-сторона: сегодня вывозится без переноса (прод-писателей нет, кейс
	// недостижим); фиксация текущего поведения для бэклога — при переносе
	// реализацией этот блок сам станет красным и обновится вместе с фиксом.
	if exists(derivedID, keepID) {
		t.Fatal("неожиданно: input-ребро переносится — обновите комментарий теста (бэклог закрыт)")
	}
	if exists(derivedID, doomedID) {
		t.Fatal("input-ребро обязано либо переноситься, либо вывозиться (FK на удалённый дубль невозможен)")
	}
}

// migrationNames — имена SQL-миграций встроенного набора: ожидаемые счётчики
// applied/skipped не хардкодятся и не ломаются от новой миграции.
func migrationNames(t *testing.T) []string {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("миграции не читаются: %v", err)
	}
	return names
}
