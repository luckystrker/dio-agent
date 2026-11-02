//go:build db

// БД-тест точной дедупликации активных записей памяти (блок 18.8): дубли
// копились как active с равным весом («поехал делать ремонт с женой» и
// «поехал с женой делать ремонт»); уникальное выражение (scope_id,
// md5(lower(btrim(text)))) WHERE status='active' + ON CONFLICT DO NOTHING
// останавливает рост — повторная идентичная вставка не плодит строку и не
// падает ошибкой.
package memory

import (
	"context"
	"testing"
)

// TestInsertDeduplicatesExactActive — повторная идентичная вставка (тот же
// скоуп, текст с точностью до регистра и краёв): одна активная строка, возврат
// без ошибки и с существующим id.
func TestInsertDeduplicatesExactActive(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	provID, err := (SourceRepo{}).EnsureProvenance(ctx, pool, SourceJournalEvent, nextRef("dedupe"), []string{scopeA})
	if err != nil {
		t.Fatalf("provenance не создан: %v", err)
	}
	candidate := NewItem{
		Kind:         KindClaim,
		Text:         "Поехал с женой делать ремонт",
		Salience:     0.6,
		ProvenanceID: provID,
	}

	first, err := (Items{}).Insert(ctx, pool, candidate, scopeA)
	if err != nil {
		t.Fatalf("первая вставка обязана проходить: %v", err)
	}

	// «Дубль»: другой регистр, пробелы по краям — тот же нормализованный текст.
	duplicate := candidate
	duplicate.Text = "  поехал с женой делать ремонт "
	second, err := (Items{}).Insert(ctx, pool, duplicate, scopeA)
	if err != nil {
		t.Fatalf("повторная идентичная вставка обязана проходить без ошибки (блок 18.8): %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_items WHERE scope_id = $1 AND status = 'active'`,
		scopeA).Scan(&n); err != nil {
		t.Fatalf("активные записи не читаются: %v", err)
	}
	if n != 1 {
		t.Fatalf("точные дубли не должны плодиться (блок 18.8): активных строк %d, хочу 1", n)
	}
	if second.ID != first.ID {
		t.Fatalf("пропуск дубля обязан возвращать существующую запись: got id=%d, хочу %d", second.ID, first.ID)
	}

	// Другой текст или чужой скоуп — не дубль: вторая строка появляется.
	other := candidate
	other.Text = "Поехал с женой выбирать обои"
	if _, err := (Items{}).Insert(ctx, pool, other, scopeA); err != nil {
		t.Fatalf("отличающийся текст обязан вставляться: %v", err)
	}
	if _, err := (Items{}).Insert(ctx, pool, candidate, scopeB); err != nil {
		t.Fatalf("тот же текст в другом скоупе обязан вставляться: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_items WHERE status = 'active'`).Scan(&n); err != nil {
		t.Fatalf("активные записи не читаются: %v", err)
	}
	if n != 3 {
		t.Fatalf("не-дубли обязаны вставляться: активных строк %d, хочу 3 (текст/скоуп различаются)", n)
	}
}
