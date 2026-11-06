//go:build db

// БД-тесты аватарок (34.2): фоновая проверка пишет описание в память скоупа
// чата, «не видно» пишется один раз, неизменившаяся аватарка не тратит
// модельный вызов, смена file_unique_id — новое описание.
package background

import (
	"context"
	"testing"

	"github.com/luckystrker/ago/events"
)

// withMessage — ответ модели обычным текстом (описание аватарки не TurnResult).
func withMessage(text string) *events.ModelResponse {
	return &events.ModelResponse{Message: &events.ModelMessage{Content: text}}
}

// fakeAvatarSource — Bot API аватарок: file_id/file_unique_id и байты.
type fakeAvatarSource struct {
	fileID, uniqueID string
	mime             string
	data             []byte
	err              error
	profileCalls     int
	fetchCalls       int
}

func (f *fakeAvatarSource) ProfilePhoto(context.Context, int64) (string, string, error) {
	f.profileCalls++
	return f.fileID, f.uniqueID, f.err
}

func (f *fakeAvatarSource) FetchAvatar(context.Context, string) (string, []byte, error) {
	f.fetchCalls++
	return f.mime, f.data, f.err
}

func TestAvatarCheckWritesMemoryAndWatchesChangeDB(t *testing.T) {
	f, ctx := newFixture(t)
	src := &fakeAvatarSource{fileID: "a1", uniqueID: "ua1", mime: "image/jpeg", data: []byte("img")}
	f.sched.avatar = src
	f.withModel(t,
		withMessage("кот в солнечных очках, весёлый"),
		withMessage("море и закат, спокойное"),
	)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO person_avatars (user_id, scope_id, chat_id, actor_name, next_check_at)
		VALUES ($1, $2, $3, 'Коля', now() - interval '1 minute')`,
		bgActor, bgScope, bgChatID); err != nil {
		t.Fatalf("очередь аватарки не создана: %v", err)
	}

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт: %v", err)
	}
	if src.profileCalls != 1 || src.fetchCalls != 1 {
		t.Fatalf("проверка аватарки: profile=%d fetch=%d, хочу 1/1", src.profileCalls, src.fetchCalls)
	}
	var text string
	if err := f.pool.QueryRow(ctx, `
		SELECT text FROM memory_items
		WHERE scope_id = $1 AND kind = 'experience' AND text LIKE '%аватарке%'
		ORDER BY id DESC LIMIT 1`, bgScope).Scan(&text); err != nil {
		t.Fatalf("описание аватарки не записано: %v", err)
	}
	if text != "у Коля на аватарке кот в солнечных очках, весёлый" {
		t.Fatalf("текст памяти = %q", text)
	}
	// Запись привязана к карточке человека (person), не к безымянной сущности.
	var linked int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_links ml
		JOIN entities e ON e.id = ml.entity_id
		WHERE e.canonical_name = 'user:888' AND e.scope_id = $1`, bgScope).Scan(&linked); err != nil || linked == 0 {
		t.Fatalf("связь с карточкой человека: %d (err=%v)", linked, err)
	}
	var (
		unique  string
		visible bool
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT file_unique_id, visible FROM person_avatars
		WHERE user_id = $1 AND scope_id = $2`, bgActor, bgScope).Scan(&unique, &visible); err != nil {
		t.Fatalf("состояние аватарки не читается: %v", err)
	}
	if unique != "ua1" || !visible {
		t.Fatalf("состояние: unique=%q visible=%v", unique, visible)
	}

	// Сутки не прошли — повторный такт не трогает ни Bot API, ни модель.
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("повторный такт: %v", err)
	}
	if src.profileCalls != 1 || src.fetchCalls != 1 {
		t.Fatalf("аватарку проверили раньше суток: profile=%d fetch=%d", src.profileCalls, src.fetchCalls)
	}

	// Смена аватарки: file_unique_id другой — новое описание.
	src.fileID, src.uniqueID = "a2", "ua2"
	if _, err := f.pool.Exec(ctx, `
		UPDATE person_avatars SET next_check_at = now() - interval '1 minute'
		WHERE user_id = $1 AND scope_id = $2`, bgActor, bgScope); err != nil {
		t.Fatalf("срок не сдвинут: %v", err)
	}
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт смены: %v", err)
	}
	if src.profileCalls != 2 || src.fetchCalls != 2 {
		t.Fatalf("смена аватарки не проверена: profile=%d fetch=%d", src.profileCalls, src.fetchCalls)
	}
	var newer string
	if err := f.pool.QueryRow(ctx, `
		SELECT text FROM memory_items
		WHERE scope_id = $1 AND kind = 'experience' AND text LIKE '%аватарке%'
		ORDER BY id DESC LIMIT 1`, bgScope).Scan(&newer); err != nil {
		t.Fatalf("новое описание не записано: %v", err)
	}
	if newer != "у Коля на аватарке море и закат, спокойное" {
		t.Fatalf("новое описание = %q", newer)
	}
}

func TestAvatarInvisibleWritesOnceDB(t *testing.T) {
	f, ctx := newFixture(t)
	src := &fakeAvatarSource{} // аватарки нет / скрыта приватностью
	f.sched.avatar = src
	f.withModel(t) // модельных ответов нет: вызов модели здесь был бы ошибкой

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO person_avatars (user_id, scope_id, chat_id, actor_name, next_check_at)
		VALUES ($1, $2, $3, 'Коля', now() - interval '1 minute')`,
		bgActor, bgScope, bgChatID); err != nil {
		t.Fatalf("очередь аватарки не создана: %v", err)
	}
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт: %v", err)
	}
	var count int
	var text string
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(min(text), '') FROM memory_items
		WHERE scope_id = $1 AND text = 'у Коля не видно аватарки'`, bgScope).Scan(&count, &text); err != nil {
		t.Fatalf("память не читается: %v", err)
	}
	if count != 1 {
		t.Fatalf("«не видно» записано %d раз, хочу 1: %q", count, text)
	}
	var visible bool
	if err := f.pool.QueryRow(ctx, `
		SELECT visible FROM person_avatars WHERE user_id = $1 AND scope_id = $2`,
		bgActor, bgScope).Scan(&visible); err != nil {
		t.Fatalf("состояние не читается: %v", err)
	}
	if visible {
		t.Fatalf("скрытая аватарка должна быть visible=false")
	}

	// Следующая проверка через сутки: «не видно» не дублируется.
	if _, err := f.pool.Exec(ctx, `
		UPDATE person_avatars SET next_check_at = now() - interval '1 minute'
		WHERE user_id = $1 AND scope_id = $2`, bgActor, bgScope); err != nil {
		t.Fatalf("срок не сдвинут: %v", err)
	}
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("повторный такт: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_items
		WHERE scope_id = $1 AND text = 'у Коля не видно аватарки'`, bgScope).Scan(&count); err != nil {
		t.Fatalf("память не читается: %v", err)
	}
	if count != 1 {
		t.Fatalf("«не видно» повторено (%d): каждый ход ретраить нельзя (34.2)", count)
	}
}

func TestAvatarUnchangedDoesNotCallModelDB(t *testing.T) {
	f, ctx := newFixture(t)
	src := &fakeAvatarSource{fileID: "a1", uniqueID: "ua1", mime: "image/jpeg", data: []byte("img")}
	f.sched.avatar = src
	f.withModel(t) // вызова модели быть не должно

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO person_avatars (user_id, scope_id, chat_id, actor_name,
			file_unique_id, visible, description, checked_at, next_check_at)
		VALUES ($1, $2, $3, 'Коля', 'ua1', true, 'кот', now() - interval '25 hours', now() - interval '1 minute')`,
		bgActor, bgScope, bgChatID); err != nil {
		t.Fatalf("состояние не создано: %v", err)
	}
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт: %v", err)
	}
	if src.profileCalls != 1 || src.fetchCalls != 0 {
		t.Fatalf("неизменившаяся аватарка: profile=%d fetch=%d, хочу 1/0", src.profileCalls, src.fetchCalls)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_items WHERE scope_id = $1 AND text LIKE '%аватарке%'`,
		bgScope).Scan(&count); err != nil {
		t.Fatalf("память не читается: %v", err)
	}
	if count != 0 {
		t.Fatalf("неизменившаяся аватарка не должна плодить записи: %d", count)
	}
}

// TestAvatarInvisibleAfterVisibleWritesOnceDB — m26 (этап 41.3): аватарка была
// описана, потом скрыта — «не видно» пишется один раз (описание прошлой
// аватарки не глушит сообщение), повтор и переходы не дублируют запись.
func TestAvatarInvisibleAfterVisibleWritesOnceDB(t *testing.T) {
	f, ctx := newFixture(t)
	src := &fakeAvatarSource{} // аватарки нет / скрыта приватностью
	f.sched.avatar = src
	f.withModel(t)

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO person_avatars
		  (user_id, scope_id, chat_id, actor_name, visible, description, next_check_at)
		VALUES ($1, $2, $3, 'Коля', true, 'на аватарке кот', now() - interval '1 minute')`,
		bgActor, bgScope, bgChatID); err != nil {
		t.Fatalf("очередь аватарки не создана: %v", err)
	}
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_items
		WHERE scope_id = $1 AND text = 'у Коля не видно аватарки'`, bgScope).Scan(&count); err != nil {
		t.Fatalf("память не читается: %v", err)
	}
	if count != 1 {
		t.Fatalf("«не видно» после описанной аватарки записано %d раз, хочу 1", count)
	}

	// Повторная проверка: маркер уже выставлен, запись не дублируется.
	if _, err := f.pool.Exec(ctx, `
		UPDATE person_avatars SET next_check_at = now() - interval '1 minute'
		WHERE user_id = $1 AND scope_id = $2`, bgActor, bgScope); err != nil {
		t.Fatalf("срок не сдвинут: %v", err)
	}
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("повторный такт: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_items
		WHERE scope_id = $1 AND text = 'у Коля не видно аватарки'`, bgScope).Scan(&count); err != nil {
		t.Fatalf("память не читается: %v", err)
	}
	if count != 1 {
		t.Fatalf("«не видно» повторено после перехода: %d", count)
	}
}
