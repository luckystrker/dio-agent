// Тесты раскадровки видео (этап 35): аргументы ffmpeg, кадры из фейкового
// скрипта, откат без ffmpeg и таймаут. Живой ffmpeg в гейте не нужен.
package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeScript кладёт исполняемый shell-скрипт во временный каталог.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg-fake")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("скрипт: %v", err)
	}
	return path
}

// fakeFFmpegScript пишет столько кадров, сколько просит шаблон: хвост
// аргументов — output-pattern вида frame_%02d.jpg.
const fakeFFmpegScript = `#!/bin/sh
last=""
for a in "$@"; do last="$a"; done
dir=$(dirname "$last")
base=$(basename "$last")
i=1
while [ $i -le 6 ]; do
  name=$(printf "$base" $i)
  printf '\xff\xd8\xff\xe0fakeframe' > "$dir/$name"
  i=$((i+1))
done
`

// TestStoryboardArgs — аргументы ffmpeg: без звука, равномерная частота кадров
// от длительности, потолок 512 px, JPEG и никакой оболочки.
func TestStoryboardArgs(t *testing.T) {
	args := StoryboardArgs("/tmp/in.mp4", "/tmp/frame_%02d.jpg", 5, 15)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-i /tmp/in.mp4",
		"-an",
		"-frames:v 5",
		"fps=5/15",
		"min(512,iw)",
		"force_original_aspect_ratio=decrease",
		"/tmp/frame_%02d.jpg",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("в аргументах нет %q: %v", want, args)
		}
	}
	for _, arg := range args {
		if strings.ContainsAny(arg, ";&|`$") {
			t.Fatalf("аргумент похож на кусок shell: %q", arg)
		}
	}
}

// TestExtractorFramesFromFakeFFmpeg — кадры читаются из вывода скрипта.
func TestExtractorFramesFromFakeFFmpeg(t *testing.T) {
	ex := NewExtractor(writeScript(t, fakeFFmpegScript))
	frames, err := ex.Frames(context.Background(), []byte("video-bytes"), 10, 3)
	if err != nil {
		t.Fatalf("кадры: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("кадров %d, хочу 3", len(frames))
	}
	for i, f := range frames {
		if len(f.Data) == 0 || f.Mime != "image/jpeg" {
			t.Fatalf("кадр %d: data=%d mime=%q", i, len(f.Data), f.Mime)
		}
	}
}

// TestExtractorDisabled — без ffmpeg раскадровка честно выключена, а не
// изображает пустой успех.
func TestExtractorDisabled(t *testing.T) {
	ex := NewExtractor("")
	if ex.Enabled() {
		t.Fatal("пустой путь не может быть включённой раскадровкой")
	}
	if _, err := ex.Frames(context.Background(), []byte("x"), 10, 3); err == nil {
		t.Fatal("без ffmpeg ожидаю ошибку")
	}
	if _, err := NewExtractor(filepath.Join(t.TempDir(), "нет-такого")).Frames(context.Background(), []byte("x"), 10, 3); err == nil {
		t.Fatal("несуществующий ffmpeg должен давать ошибку")
	}
}

// TestExtractorTimeout — зависший ffmpeg обрывается таймаутом, ход не ждёт.
func TestExtractorTimeout(t *testing.T) {
	ex := NewExtractor(writeScript(t, "#!/bin/sh\nexec sleep 5\n"))
	ex.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := ex.Frames(context.Background(), []byte("video-bytes"), 10, 3)
	if err == nil {
		t.Fatal("зависший ffmpeg должен вернуть ошибку таймаута")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("таймаут не сработал: ждали %v", elapsed)
	}
}

// TestExtractorRejectsBadInput — пустые видео и нулевая длительность не
// запускают ffmpeg (дешёвый отказ до процесса).
func TestExtractorRejectsBadInput(t *testing.T) {
	ex := NewExtractor(writeScript(t, fakeFFmpegScript))
	if _, err := ex.Frames(context.Background(), nil, 10, 5); err == nil {
		t.Fatal("пустое видео должно отклоняться")
	}
	if _, err := ex.Frames(context.Background(), []byte("x"), 0, 5); err == nil {
		t.Fatal("нулевая длительность должна отклоняться")
	}
}
