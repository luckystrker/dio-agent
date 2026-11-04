// Тесты раскадровки на стороне персонажа (этап 35): кадры гифки/кружка идут
// в модель вместо одного превью, общий потолок картинок хода соблюдается,
// явные фото/стикеры приоритетнее, а сбой/отсутствие ffmpeg откатываются на
// превью-кадр с честной пометкой в логе.
package character

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/luckystrker/dio-agent/internal/media"
	"github.com/luckystrker/dio-agent/internal/telegram"
)

// fakeVideoFetcher отдаёт видео по любому file_id и запоминает запросы.
type fakeVideoFetcher struct {
	ids  []string
	data []byte
	err  error
}

func (f *fakeVideoFetcher) FetchVideoByFileID(_ context.Context, fileID string) (*telegram.VideoMedia, error) {
	f.ids = append(f.ids, fileID)
	if f.err != nil {
		return nil, f.err
	}
	return &telegram.VideoMedia{Data: f.data}, nil
}

// idPhotoFetcher запоминает file_id скачанных картинок.
type idPhotoFetcher struct {
	ids []string
}

func (f *idPhotoFetcher) FetchPhotoByFileID(_ context.Context, fileID string) (*telegram.PhotoMedia, error) {
	f.ids = append(f.ids, fileID)
	return &telegram.PhotoMedia{Mime: "image/jpeg", Base64: "eHk="}, nil
}

// fakeStoryboarder пишет want кадров; err/empty имитируют сбой ffmpeg.
type fakeStoryboarder struct {
	calls []int
	want  int
	err   error
}

func (f *fakeStoryboarder) Frames(_ context.Context, _ []byte, _ int, want int) ([]media.Frame, error) {
	f.calls = append(f.calls, want)
	if f.err != nil {
		return nil, f.err
	}
	n := want
	if f.want > 0 {
		n = f.want
	}
	out := make([]media.Frame, n)
	for i := range out {
		out[i] = media.Frame{Data: []byte{0xff, 0xd8, byte(i)}, Mime: "image/jpeg"}
	}
	return out, nil
}

const gifRaw = `{"message":{"animation":{"file_id":"gif","duration":10,"thumbnail":{"file_id":"gif-thumb"}}}}`

// TestCollectVisualsStoryboardReplacesPreview — гифка даёт 5 кадров, а её
// превью в запрос не дублируется.
func TestCollectVisualsStoryboardReplacesPreview(t *testing.T) {
	photos := &idPhotoFetcher{}
	videos := &fakeVideoFetcher{data: []byte("mp4")}
	board := &fakeStoryboarder{}
	var logs []string

	got := CollectVisuals(context.Background(), []BatchEvent{{EventID: 7, Raw: json.RawMessage(gifRaw)}},
		photos, videos, board, 6, func(format string, args ...any) {
			logs = append(logs, format)
		})
	if len(videos.ids) != 1 || videos.ids[0] != "gif" {
		t.Fatalf("видео не скачано: %v", videos.ids)
	}
	if len(got.Images) != 5 {
		t.Fatalf("картинок %d, хочу 5 кадров", len(got.Images))
	}
	if got.StoryboardFrames[7] != 5 {
		t.Fatalf("кадров события = %d, хочу 5", got.StoryboardFrames[7])
	}
	if len(photos.ids) != 0 {
		t.Fatalf("превью гифки не должно скачиваться при успешной раскадровке: %v", photos.ids)
	}
	if len(logs) == 0 {
		t.Fatal("успешная раскадровка должна оставлять строку в логе")
	}
}

// TestCollectVisualsPhotosTakePriority — явные фото пачки идут первыми и
// занимают свой потолок (3); раскадровке остаётся остаток общего потолка (6).
func TestCollectVisualsPhotosTakePriority(t *testing.T) {
	batch := []BatchEvent{
		{EventID: 1, Raw: json.RawMessage(`{"message":{"photo":[{"file_id":"p1","width":9,"height":9}]}}`)},
		{EventID: 2, Raw: json.RawMessage(`{"message":{"photo":[{"file_id":"p2","width":9,"height":9}]}}`)},
		{EventID: 3, Raw: json.RawMessage(`{"message":{"photo":[{"file_id":"p3","width":9,"height":9}]}}`)},
		{EventID: 4, Raw: json.RawMessage(`{"message":{"photo":[{"file_id":"p4","width":9,"height":9}]}}`)},
		{EventID: 5, Raw: json.RawMessage(gifRaw)},
	}
	photos := &idPhotoFetcher{}
	board := &fakeStoryboarder{}
	got := CollectVisuals(context.Background(), batch, photos,
		&fakeVideoFetcher{data: []byte("mp4")}, board, 6, nil)

	if len(photos.ids) != 3 {
		t.Fatalf("фото пачки %d, хочу 3 (прежний потолок явных картинок): %v", len(photos.ids), photos.ids)
	}
	if len(got.Images) != 6 {
		t.Fatalf("всего картинок %d, хочу 6 (3 фото + 3 кадра)", len(got.Images))
	}
	if got.StoryboardFrames[5] != 3 {
		t.Fatalf("кадров гифки = %d, хочу 3", got.StoryboardFrames[5])
	}
}

// TestCollectVisualsTwoVideosShareBudget — две гифки делят остаток потолка,
// ни одна не остаётся совсем без кадров.
func TestCollectVisualsTwoVideosShareBudget(t *testing.T) {
	batch := []BatchEvent{
		{EventID: 1, Raw: json.RawMessage(gifRaw)},
		{EventID: 2, Raw: json.RawMessage(gifRaw)},
	}
	board := &fakeStoryboarder{}
	got := CollectVisuals(context.Background(), batch, &idPhotoFetcher{},
		&fakeVideoFetcher{data: []byte("mp4")}, board, 6, nil)
	if len(got.Images) != 6 {
		t.Fatalf("картинок %d, хочу 6", len(got.Images))
	}
	if got.StoryboardFrames[1] != 3 || got.StoryboardFrames[2] != 3 {
		t.Fatalf("кадры по гифкам = %v, хочу по 3", got.StoryboardFrames)
	}
}

// TestCollectVisualsFallsBackToPreview — нет ffmpeg или он упал: остаётся
// превью-кадр, пометки раскадровки нет, причина в логе.
func TestCollectVisualsFallsBackToPreview(t *testing.T) {
	batch := []BatchEvent{{EventID: 7, Raw: json.RawMessage(gifRaw)}}

	t.Run("без ffmpeg", func(t *testing.T) {
		photos := &idPhotoFetcher{}
		got := CollectVisuals(context.Background(), batch, photos,
			&fakeVideoFetcher{data: []byte("mp4")}, nil, 6, nil)
		if len(photos.ids) != 1 || photos.ids[0] != "gif-thumb" {
			t.Fatalf("ожидал превью-кадр: %v", photos.ids)
		}
		if len(got.StoryboardFrames) != 0 {
			t.Fatalf("пометки раскадровки быть не должно: %v", got.StoryboardFrames)
		}
	})

	t.Run("ffmpeg упал", func(t *testing.T) {
		photos := &idPhotoFetcher{}
		var logged bool
		got := CollectVisuals(context.Background(), batch, photos,
			&fakeVideoFetcher{data: []byte("mp4")},
			&fakeStoryboarder{err: errors.New("ffmpeg: boom")}, 6,
			func(format string, _ ...any) {
				if format == "" {
					t.Error("пустая строка лога")
				}
				logged = true
			})
		if !logged {
			t.Fatal("сбой раскадровки должен попадать в лог")
		}
		if len(photos.ids) != 1 || photos.ids[0] != "gif-thumb" {
			t.Fatalf("ожидал откат на превью: %v", photos.ids)
		}
		if len(got.StoryboardFrames) != 0 {
			t.Fatalf("пометки раскадровки быть не должно: %v", got.StoryboardFrames)
		}
	})

	t.Run("видео слишком длинное", func(t *testing.T) {
		long := `{"message":{"video_note":{"file_id":"n","duration":600,"thumbnail":{"file_id":"n-thumb"}}}}`
		photos := &idPhotoFetcher{}
		got := CollectVisuals(context.Background(), []BatchEvent{{EventID: 8, Raw: json.RawMessage(long)}},
			photos, &fakeVideoFetcher{data: []byte("mp4")}, &fakeStoryboarder{}, 6, nil)
		if len(photos.ids) != 1 || photos.ids[0] != "n-thumb" {
			t.Fatalf("длинное видео — только превью кружка: %v", photos.ids)
		}
		if len(got.StoryboardFrames) != 0 {
			t.Fatalf("раскадровки длинного видео быть не должно: %v", got.StoryboardFrames)
		}
	})
}

// TestAnnotateStoryboards — пометка кадров попадает в текст пачки до запроса
// модели: гифка говорит «раскадровка 5 кадров», кружок — со звуком.
func TestAnnotateStoryboards(t *testing.T) {
	batch := []BatchEvent{
		{EventID: 1, Raw: json.RawMessage(gifRaw)},
		{EventID: 2, Raw: json.RawMessage(`{"message":{"video_note":{"file_id":"n","duration":15}}}`)},
		{EventID: 3, Raw: json.RawMessage(`{"message":{"text":"привет"}}`)},
	}
	texts := []string{"[гифка]", "[кружок, 0:15]", "привет"}
	got := annotateStoryboards(texts, batch, map[int64]int{1: 5, 2: 3})
	want := []string{
		"[гифка, раскадровка 5 кадров]",
		"[кружок, 0:15, раскадровка 3 кадра — звук не слышишь]",
		"привет",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("строка %d = %q, хочу %q", i, got[i], want[i])
		}
	}
}
