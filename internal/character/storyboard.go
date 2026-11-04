// Раскадровка гифок и кружков на стороне хода (этап 35): видео скачивается
// целиком (telegram.VideoFetcher), внешний ffmpeg (media.Extractor) режет его
// на кадры, а кадры идут модели обычными картинками. Текст события получает
// пометку «раскадровка N кадров» — до запроса модели.
//
// Общий потолок картинок хода (telegram.MaxImagesPerTurn) один для всех:
// фото/стикеры пачки по-прежнему занимают свой потолок (3) — это цельные
// сообщения, а раскадровка добирает остаток. Если видео два, остаток делится
// между ними; сбой/отсутствие ffmpeg — откат на превью-кадр без пометки.
package character

import (
	"context"
	"fmt"

	"github.com/luckystrker/dio-agent/internal/media"
	"github.com/luckystrker/dio-agent/internal/telegram"
)

const (
	// maxNonVideoImages — прежний потолок явных картинок пачки (фото и
	// статичные стикеры): их смысл не восстанавливается текстом, поэтому они
	// приоритетнее кадров раскадровки.
	maxNonVideoImages = 3
	// maxStoryboardDuration — видео длиннее не раскадровываем: 4–6 кадров всё
	// равно не покажут сюжет, а время и трафик растут.
	maxStoryboardDuration = 120 // секунды
	// storyboardTargetFrames — сколько кадров хочется с одного видео (4–6).
	storyboardTargetFrames = media.TargetFrames
)

// VideoFetcher — скачивание видео по file_id; nil — раскадровки нет.
type VideoFetcher interface {
	FetchVideoByFileID(ctx context.Context, fileID string) (*telegram.VideoMedia, error)
}

// StoryboardExtractor — ffmpeg-раскадровка (media.Extractor в проде).
type StoryboardExtractor interface {
	Frames(ctx context.Context, video []byte, durationSeconds, want int) ([]media.Frame, error)
}

// Visuals — картинки хода и сведения о раскадровках: EventID → число кадров,
// реально ушедших модели (для пометки в тексте события).
type Visuals struct {
	Images           []Photo
	StoryboardFrames map[int64]int
}

// visualSlot — место события в порядке пачки: обычная картинка или видео.
type visualSlot struct {
	event   BatchEvent
	video   *telegram.VideoInfo
	fileID  string // картинка слота: фото/стикер/превью (не видео)
	frames  []media.Frame
	dropped bool // картинки не будет (потолок исчерпан)
}

// CollectVisuals собирает картинки пачки с раскадровкой видео. max — общий
// потолок картинок хода (telegram.MaxImagesPerTurn); ошибки скачивания и
// ffmpeg не роняют ход: событие остаётся текстом или превью-кадром.
func CollectVisuals(ctx context.Context, batch []BatchEvent, photos PhotoFetcher, videos VideoFetcher, storyboard StoryboardExtractor, max int, logf func(format string, args ...any)) Visuals {
	out := Visuals{StoryboardFrames: map[int64]int{}}
	if max <= 0 {
		max = telegram.MaxImagesPerTurn
	}

	slots := make([]visualSlot, 0, len(batch))
	ordinary := 0
	for _, ev := range batch {
		info := telegram.VideoFromUpdate(ev.Raw)
		if info != nil && videos != nil && storyboard != nil {
			if info.Duration <= 0 {
				logfStoryboard(logf, "раскадровка: %s — длительность неизвестна, превью-кадр", videoLabel(info))
			} else if info.Duration > maxStoryboardDuration {
				logfStoryboard(logf, "раскадровка: %s длиннее %d с — превью-кадр", videoLabel(info), maxStoryboardDuration)
			} else {
				slots = append(slots, visualSlot{event: ev, video: info})
				continue
			}
		}
		if ordinary >= maxNonVideoImages {
			continue
		}
		if fileID := imageFileIDFromBatchEvent(ev); fileID != "" {
			slots = append(slots, visualSlot{event: ev, fileID: fileID})
			ordinary++
		}
	}

	// Кадры видео: явные картинки уже заняли ordinary слотов, раскадровка
	// делит остаток потолка между видео.
	used := ordinary
	videoIndexes := make([]int, 0, len(slots))
	for i := range slots {
		if slots[i].video != nil {
			videoIndexes = append(videoIndexes, i)
		}
	}
	for n, index := range videoIndexes {
		slot := &slots[index]
		left := len(videoIndexes) - n
		if used >= max {
			// Потолок исчерпан — превью-кадр тоже не лезет; текст события
			// расскажет, что было видео.
			slot.dropped = true
			continue
		}
		want := (max - used) / left
		if want > storyboardTargetFrames {
			want = storyboardTargetFrames
		}
		if want < 1 {
			want = 1
		}
		video, err := videos.FetchVideoByFileID(ctx, slot.video.FileID)
		if err != nil || video == nil {
			logfStoryboard(logf, "раскадровка: %s — видео не скачано (%v), превью-кадр", videoLabel(slot.video), err)
			slot.fileID = previewFileID(slot.video)
			if slot.fileID != "" {
				used++
			} else {
				slot.dropped = true
			}
			continue
		}
		frames, ferr := storyboard.Frames(ctx, video.Data, slot.video.Duration, want)
		if ferr != nil || len(frames) == 0 {
			logfStoryboard(logf, "раскадровка: %s — ffmpeg не справился (%v), превью-кадр", videoLabel(slot.video), ferr)
			slot.fileID = previewFileID(slot.video)
			if slot.fileID != "" {
				used++
			} else {
				slot.dropped = true
			}
			continue
		}
		slot.frames = frames
		out.StoryboardFrames[slot.event.EventID] = len(frames)
		used += len(frames)
		logfStoryboard(logf, "раскадровка: %s — %d кадров", videoLabel(slot.video), len(frames))
	}

	// Разворачиваем слоты в порядке пачки: текст Ask и картинки сохраняют
	// последовательность событий.
	for _, slot := range slots {
		if len(slot.frames) > 0 {
			for _, frame := range slot.frames {
				mime := frame.Mime
				if mime == "" {
					mime = "image/jpeg"
				}
				out.Images = append(out.Images, Photo{FileID: slot.video.FileID, Data: frame.Data, Mime: mime})
			}
			continue
		}
		if slot.fileID == "" || slot.dropped || photos == nil {
			continue
		}
		pm, err := photos.FetchPhotoByFileID(ctx, slot.fileID)
		if err != nil || pm == nil {
			logfStoryboard(logf, "картинка не скачана — событие остаётся текстом (event %d): err=%v пусто=%v", slot.event.EventID, err, pm == nil)
			continue
		}
		out.Images = append(out.Images, Photo{FileID: slot.fileID, Data: pm.Data(), Mime: pm.Mime})
	}
	return out
}

// previewFileID — превью видео для отката: у гифки и кружка это thumbnail.
func previewFileID(info *telegram.VideoInfo) string {
	if info == nil {
		return ""
	}
	return info.ThumbnailFileID
}

// videoLabel — «гифка 0:12» / «кружок 0:15» для строк лога.
func videoLabel(info *telegram.VideoInfo) string {
	if info == nil {
		return "видео"
	}
	kind := "гифка"
	if info.Kind == telegram.VideoKindVideoNote {
		kind = "кружок"
	}
	return kind + " " + storyboardDuration(info.Duration)
}

// storyboardDuration — «0:15» для строк лога.
func storyboardDuration(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

// annotateStoryboards подставляет пометку раскадровки в тексты пачки: bctx.Batch
// и batch идут в одном порядке, EventID связывает их с числом кадров.
func annotateStoryboards(texts []string, batch []BatchEvent, frames map[int64]int) []string {
	if len(frames) == 0 || len(texts) == 0 {
		return texts
	}
	out := append([]string(nil), texts...)
	limit := len(batch)
	if len(out) < limit {
		limit = len(out)
	}
	for i := 0; i < limit; i++ {
		count := frames[batch[i].EventID]
		if count <= 0 {
			continue
		}
		info := telegram.VideoFromUpdate(batch[i].Raw)
		if info == nil {
			continue
		}
		out[i] = telegram.AnnotateStoryboard(out[i], info, count)
	}
	return out
}

// logfStoryboard — безопасный лог: nil-функция означает «молча».
func logfStoryboard(logf func(format string, args ...any), format string, args ...any) {
	if logf != nil {
		logf(format, args...)
	}
}
