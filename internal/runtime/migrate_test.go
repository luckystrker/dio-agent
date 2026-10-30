package runtime

import (
	"testing"
	"testing/fstest"
)

// Юнит-тесты раннера без БД: порядок миграций и отбор неприменённых.
// Полное применение — в migrate_db_test.go (тег db, против docker-compose Postgres).

func TestListMigrationsSortedAndFiltered(t *testing.T) {
	fsys := fstest.MapFS{
		"000003_c.sql": &fstest.MapFile{Data: []byte("SELECT 3;")},
		"000001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		"000002_b.txt": &fstest.MapFile{Data: []byte("не миграция")},
		"README.md":    &fstest.MapFile{Data: []byte("читай меня")},
	}
	got, err := ListMigrations(fsys)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	want := []string{"000001_a.sql", "000003_c.sql"}
	if len(got) != len(want) {
		t.Fatalf("получено %d миграций, хочу %d: %+v", len(got), len(want), got)
	}
	for i, m := range got {
		if m.Name != want[i] {
			t.Errorf("позиция %d: name = %q, хочу %q", i, m.Name, want[i])
		}
	}
	if got[0].SQL != "SELECT 1;" {
		t.Errorf("SQL первой миграции = %q", got[0].SQL)
	}
}

func TestListMigrationsRejectsEmptySQL(t *testing.T) {
	fsys := fstest.MapFS{"000001_empty.sql": &fstest.MapFile{Data: []byte("   \n")}}
	if _, err := ListMigrations(fsys); err == nil {
		t.Error("пустая миграция должна давать явную ошибку")
	}
}

func TestListMigrationsRejectsNothingFound(t *testing.T) {
	if _, err := ListMigrations(fstest.MapFS{}); err == nil {
		t.Error("отсутствие миграций должно давать явную ошибку")
	}
}

func TestPendingMigrationsFiltersApplied(t *testing.T) {
	all := []Migration{
		{Name: "000001_a.sql"},
		{Name: "000002_b.sql"},
		{Name: "000003_c.sql"},
	}
	applied := map[string]bool{"000001_a.sql": true, "000003_c.sql": true}
	pending := PendingMigrations(all, applied)
	if len(pending) != 1 || pending[0].Name != "000002_b.sql" {
		t.Errorf("pending = %+v, хочу только 000002_b.sql", pending)
	}
}
