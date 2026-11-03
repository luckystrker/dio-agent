// Этап 24.3: честность вымышленной жизни. Строка о том, что события жизни —
// придуманные, живёт в статичном system-промпте (не ломает кэш префикса):
// рассказывать их как прожитое можно, но на прямой вопрос «это правда было?»
// персонаж обязан честно сказать, что он ИИ и это его придуманная жизнь.
package character

import (
	"strings"
	"testing"
)

func TestLifeEventHonestyIsStatic(t *testing.T) {
	sources := map[string]string{
		"config/system-prompt.md": readRepoFile(t, "../../config/system-prompt.md"),
		"встроенный дефолт":       DefaultSystemPromptTemplate,
	}
	for name, text := range sources {
		low := strings.ToLower(text)
		for _, want := range []string{"вымышлен", "придуманная жизнь", "я ии"} {
			if !strings.Contains(low, want) {
				t.Fatalf("%s: нет строки о вымысле событий жизни (ищу %q)", name, want)
			}
		}
	}
}
