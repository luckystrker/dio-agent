// Раскадровка видео (этап 35): гифка и кружок — это движение, и одного
// превью-кадра мало. Внешний ffmpeg режет видео на 4–6 равномерных JPEG-кадров
// (≤512 px, без звука), которые уходят модели обычными картинками хода.
//
// Инструмент внешний и необязательный: нет ffmpeg — раскадровка честно
// выключена, работа идёт по превью-кадру из Telegram. Запуск только через
// exec.CommandContext с нашими аргументами (без shell), файлы — во временном
// каталоге os.MkdirTemp, который удаляется после прогона.
package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultTimeout — потолок одного прогона ffmpeg: ход не должен ждать
	// раскадровку дольше.
	DefaultTimeout = 15 * time.Second
	// TargetFrames — сколько кадров хочется получить с одного видео (4–6).
	TargetFrames = 5
	// MaxFrames — верхняя граница кадров с одного видео.
	MaxFrames = 6
	// maxFrameSide — кадр ужимается до этой стороны (меньшее не растягиваем).
	maxFrameSide = 512
	// versionTimeout — проверка версии при старте не задерживает запуск.
	versionTimeout = 5 * time.Second
)

// ErrDisabled — ffmpeg не настроен/не найден: раскадровка выключена.
var ErrDisabled = errors.New("раскадровка выключена: нет ffmpeg")

// Frame — один JPEG-кадр раскадровки.
type Frame struct {
	Data []byte
	Mime string
}

// Extractor запускает ffmpeg. Path — уже разрешённый путь (runtime.Config).
type Extractor struct {
	Path    string
	Timeout time.Duration
}

// NewExtractor — экстрактор по пути; пустой путь — выключен.
func NewExtractor(path string) *Extractor {
	return &Extractor{Path: strings.TrimSpace(path), Timeout: DefaultTimeout}
}

// Enabled — ffmpeg есть, раскадровка возможна.
func (e *Extractor) Enabled() bool {
	return e != nil && strings.TrimSpace(e.Path) != ""
}

// Version — первая строка `ffmpeg -version` для лога запуска; пусто/ошибка —
// «версия не определена» (раскадровка от этого не зависит).
func (e *Extractor) Version(ctx context.Context) string {
	if !e.Enabled() {
		return ""
	}
	vctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(vctx, e.Path, "-version").Output()
	if err != nil {
		return "версия не определена"
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return line
}

// Frames режет видео на want кадров (по умолчанию TargetFrames). Ошибка —
// повод откатиться на превью-кадр, а не уронить ход.
func (e *Extractor) Frames(ctx context.Context, video []byte, durationSeconds, want int) ([]Frame, error) {
	if !e.Enabled() {
		return nil, ErrDisabled
	}
	if len(video) == 0 {
		return nil, errors.New("раскадровка: пустое видео")
	}
	if durationSeconds <= 0 {
		return nil, errors.New("раскадровка: длительность видео неизвестна")
	}
	if want <= 0 {
		want = TargetFrames
	}
	if want > MaxFrames {
		want = MaxFrames
	}

	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "dio-storyboard-")
	if err != nil {
		return nil, fmt.Errorf("раскадровка: временный каталог: %w", err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // уборка временных файлов

	input := filepath.Join(dir, "source.mp4")
	if err := os.WriteFile(input, video, 0o600); err != nil {
		return nil, fmt.Errorf("раскадровка: видео не записано: %w", err)
	}
	pattern := filepath.Join(dir, "frame_%02d.jpg")

	cmd := exec.CommandContext(fctx, e.Path, StoryboardArgs(input, pattern, want, durationSeconds)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(fctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("раскадровка: ffmpeg не успел за %s: %w", timeout, fctx.Err())
		}
		return nil, fmt.Errorf("раскадровка: ffmpeg: %v: %s", err, shorten(string(output), 400))
	}

	frames := make([]Frame, 0, want)
	for i := 1; i <= want; i++ {
		data, readErr := os.ReadFile(filepath.Join(dir, fmt.Sprintf("frame_%02d.jpg", i)))
		if readErr != nil || len(data) == 0 {
			break
		}
		frames = append(frames, Frame{Data: data, Mime: "image/jpeg"})
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("раскадровка: ffmpeg не оставил кадров: %s", shorten(string(output), 400))
	}
	return frames, nil
}

// StoryboardArgs — аргументы ffmpeg: равномерная частота кадров от известной
// длительности, без звука, кадр ≤512 px по каждой стороне (меньшее видео не
// растягиваем), JPEG. Никакой оболочки — пути и числа передаются как аргументы.
func StoryboardArgs(inputPath, outputPattern string, frames, durationSeconds int) []string {
	if frames <= 0 {
		frames = TargetFrames
	}
	if durationSeconds <= 0 {
		durationSeconds = 1
	}
	filter := fmt.Sprintf(
		"fps=%d/%d,scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease",
		frames, durationSeconds, maxFrameSide, maxFrameSide,
	)
	return []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-i", inputPath,
		"-an",
		"-vf", filter,
		"-frames:v", strconv.Itoa(frames),
		"-q:v", "3",
		"-y", outputPattern,
	}
}

// shorten — хвост stderr ffmpeg для причины в логе.
func shorten(text string, limit int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
