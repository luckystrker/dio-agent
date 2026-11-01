package memory

import (
	"context"
	"testing"
)

// TestToolsetIndexPendingWithoutEmbedder — векторизатор не настроен:
// индексация пропускается без ошибки (16.8), семантика честно выключена.
func TestToolsetIndexPendingWithoutEmbedder(t *testing.T) {
	ts := &Toolset{search: &Searcher{}, items: Items{}}
	n, err := ts.IndexPending(context.Background(), 32)
	if err != nil || n != 0 {
		t.Fatalf("IndexPending без векторизатора: n=%d err=%v, хочу 0/<nil>", n, err)
	}
}
