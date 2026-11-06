// Юнит-тест имени фоновой задачи для дампа модели (40.1): operation теряет
// служебный префикс, остальное (включая двоеточия) едет в имя и санитизируется
// уже дампером.
package background

import "testing"

func TestDumpTaskStripsModelPrefix(t *testing.T) {
	cases := map[string]string{
		"model:bg:intention:42:1": "intention:42:1",
		"model:bg:consolidation":  "consolidation",
		"test:reasoning:bg":       "test:reasoning:bg",
		"":                        "",
	}
	for in, want := range cases {
		if got := dumpTask(in); got != want {
			t.Fatalf("dumpTask(%q) = %q, хочу %q", in, got, want)
		}
	}
}
