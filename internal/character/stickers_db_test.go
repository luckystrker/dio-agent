//go:build db

// БД-тесты коллекции стикеров (25.3/25.5): синхронизация стартового набора из
// файла персоны, поиск по метке, сохранение присланного стикера и отсечение
// дублей по file_unique_id.
package character

import (
	"context"
	"strings"
	"testing"

	"github.com/luckystrker/dio-agent/internal/telegram"
)

func TestStickerCollectionPersonaSync(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewStickerRepo()

	entries := []SourceSticker{
		{FileID: "P1", Emoji: "😂", Note: "ржу"},
		{FileID: "P2", Emoji: "🖕", Note: "послать"},
	}
	n, err := repo.SyncPersona(ctx, pool, entries)
	if err != nil || n != 2 {
		t.Fatalf("синхронизация персоны: n=%d err=%v", n, err)
	}
	list, err := repo.List(ctx, pool, 20)
	if err != nil || len(list) != 2 {
		t.Fatalf("список коллекции: %v %v", list, err)
	}
	for _, s := range list {
		if s.Source != StickerSourcePersona {
			t.Fatalf("источник стикера %q, хочу persona", s.Source)
		}
	}

	// Повторная синхронизация тех же строк ничего не дублирует; заметка
	// обновляется по file_id; удалённая из файла строка исчезает.
	updated := []SourceSticker{{FileID: "P1", Emoji: "😂", Note: "ржу громче"}}
	n, err = repo.SyncPersona(ctx, pool, updated)
	if err != nil || n != 1 {
		t.Fatalf("повторная синхронизация: n=%d err=%v", n, err)
	}
	list, err = repo.List(ctx, pool, 20)
	if err != nil || len(list) != 1 {
		t.Fatalf("после сужения набора ожидался 1 стикер: %v %v", list, err)
	}
	if list[0].FileID != "P1" || list[0].Note != "ржу громче" {
		t.Fatalf("заметка не обновлена: %+v", list[0])
	}

	// Сохранённые (source=saved) строки синхронизация не трогает.
	savedID, created, err := repo.Save(ctx, pool, telegram.StickerInfo{
		FileID: "S1", FileUniqueID: "SU1", Emoji: "🔥", SetName: "pack",
	}, "когда жарко")
	if err != nil || !created || savedID == 0 {
		t.Fatalf("сохранение стикера: id=%d created=%v err=%v", savedID, created, err)
	}
	if _, err := repo.SyncPersona(ctx, pool, nil); err != nil {
		t.Fatalf("пустая персона: %v", err)
	}
	list, err = repo.List(ctx, pool, 20)
	if err != nil || len(list) != 1 || list[0].FileID != "S1" {
		t.Fatalf("сохранённый стикер потерян синхронизацией персоны: %v %v", list, err)
	}
}

func TestStickerLabelAndSaveDedupe(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewStickerRepo()

	if _, err := repo.SyncPersona(ctx, pool, []SourceSticker{{FileID: "P1", Emoji: "😂", Note: "ржу"}}); err != nil {
		t.Fatalf("синхронизация: %v", err)
	}
	list, err := repo.List(ctx, pool, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("список: %v %v", list, err)
	}
	label := stickerLabel(list[0].ID)

	// Метка из панели находит тот же стикер; чужая метка — nil без ошибки.
	found, err := repo.ByLabel(ctx, pool, label)
	if err != nil || found == nil || found.FileID != "P1" {
		t.Fatalf("метка %s не нашла стикер: %+v %v", label, found, err)
	}
	missing, err := repo.ByLabel(ctx, pool, "#99999")
	if err != nil || missing != nil {
		t.Fatalf("несуществующая метка: %+v %v", missing, err)
	}

	// Первое появление стикера персоны в пачке дорисовывает file_unique_id:
	// без него дедупликация «сохраню себе» не работала бы.
	id, created, err := repo.Save(ctx, pool, telegram.StickerInfo{
		FileID: "P1", FileUniqueID: "PU1", Emoji: "😂",
	}, "ржу")
	if err != nil || created || id != list[0].ID {
		t.Fatalf("стикер персоны не узнан: id=%d created=%v err=%v", id, created, err)
	}
	var unique string
	if err := pool.QueryRow(ctx, `SELECT file_unique_id FROM stickers WHERE id = $1`, id).Scan(&unique); err != nil || unique != "PU1" {
		t.Fatalf("file_unique_id не дорисован: %q %v", unique, err)
	}

	// Сохранение нового: один раз создаёт, повтор — дубль отсечён.
	_, created, err = repo.Save(ctx, pool, telegram.StickerInfo{FileID: "N1", FileUniqueID: "NU1", Emoji: "🔥"}, "огонь")
	if err != nil || !created {
		t.Fatalf("новый стикер не сохранён: created=%v err=%v", created, err)
	}
	dupID, created, err := repo.Save(ctx, pool, telegram.StickerInfo{FileID: "N1b", FileUniqueID: "NU1", Emoji: "🔥"}, "огонь снова")
	if err != nil || created || dupID == 0 {
		t.Fatalf("дубль по file_unique_id не отсечён: id=%d created=%v err=%v", dupID, created, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM stickers WHERE file_unique_id = 'NU1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("строк с NU1: %d err=%v", count, err)
	}
	// Стикер без file_unique_id (персона до встречи) не сохраняется как saved:
	// дублей по file_id быть не должно.
	before, _ := repo.List(ctx, pool, 50)
	if _, _, err := repo.Save(ctx, pool, telegram.StickerInfo{FileID: "P1"}, "без уникального"); err != nil {
		t.Fatalf("сохранение без unique: %v", err)
	}
	after, _ := repo.List(ctx, pool, 50)
	if len(after) != len(before) {
		t.Fatalf("стикер без file_unique_id размножился: было %d, стало %d", len(before), len(after))
	}
}

// TestPanelShowsStickerCollection — коллекция видна модели в панели состояния:
// метка, эмодзи и заметка «когда уместен»; без коллекции строки нет.
func TestPanelShowsStickerCollection(t *testing.T) {
	f, ctx := newFixture(t)
	f.runner.Assembler.Stickers = NewStickerRepo()
	batch := []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "привет"}}

	before, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, batch, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	if strings.Contains(before.StatePanel, "твои стикеры") {
		t.Fatalf("пустая коллекция попала в панель:\n%s", before.StatePanel)
	}

	n, err := NewStickerRepo().SyncPersona(ctx, f.pool, []SourceSticker{
		{FileID: "P1", Emoji: "😂", Note: "ржу над тупой шуткой"},
	})
	if err != nil || n != 1 {
		t.Fatalf("набор персоны: n=%d err=%v", n, err)
	}
	list, err := NewStickerRepo().List(ctx, f.pool, 5)
	if err != nil || len(list) != 1 {
		t.Fatalf("коллекция: %v %v", list, err)
	}

	after, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, batch, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	for _, want := range []string{"твои стикеры", stickerLabel(list[0].ID), "😂", "ржу над тупой шуткой", "sticker"} {
		if !strings.Contains(after.StatePanel, want) {
			t.Fatalf("в панели нет %q:\n%s", want, after.StatePanel)
		}
	}
}
