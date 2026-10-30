package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotEnvAppliesAndSkips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# комментарий\n" +
		"DTO_AGENT_TEST_A=hello\n" +
		"export DTO_AGENT_TEST_B=world\n" +
		"  DTO_AGENT_TEST_C =  spaced  \n" +
		"DTO_AGENT_TEST_D=\"quoted value\"\n" +
		"DTO_AGENT_TEST_E='single'\n" +
		"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DTO_AGENT_TEST_C", "existing") // окружение сильнее файла

	if err := LoadDotEnv(path); err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	for key, want := range map[string]string{
		"DTO_AGENT_TEST_A": "hello",
		"DTO_AGENT_TEST_B": "world",
		"DTO_AGENT_TEST_C": "existing", // не переопределён
		"DTO_AGENT_TEST_D": "quoted value",
		"DTO_AGENT_TEST_E": "single",
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, хочу %q", key, got, want)
		}
	}
}

func TestLoadDotEnvMissingFileIsFine(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "нет-такого-файла")); err != nil {
		t.Errorf("отсутствие .env — не ошибка, получено: %v", err)
	}
}

func TestLoadDotEnvMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("БЕЗРАВНО\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := LoadDotEnv(path)
	if err == nil || !strings.Contains(err.Error(), "строка 1") {
		t.Errorf("битая строка должна давать явную ошибку с номером строки, получено: %v", err)
	}
}
