// Загрузка файла .env для локальной разработки.
//
// В проде переменные задаёт systemd (EnvironmentFile, см. RUNBOOK §6), здесь
// .env — паритет с TS-версией, где dotenv подмешивал файл в окружение.
// Зависимости не привлекаем (философия №6): ~60 строк stdlib.
//
// Правила: KEY=VALUE; пробелы по краям срезаются; комментарии от #; префикс
// export допустим; парные кавычки вокруг значения снимаются. Уже установленные
// переменные окружения НЕ переопределяются (как у dotenv): окружение сильнее
// файла. Отсутствие файла — не ошибка; нечитаемая или битая строка — явная.
package runtime

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadDotEnv читает файл path и подмешивает переменные в окружение процесса.
// Возвращает nil, если файла нет.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf(".env: %w", err)
	}
	defer f.Close() //nolint:errcheck // чтение уже завершено; закрытие не влияет на результат

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found {
			return fmt.Errorf(".env: строка %d: ожидаю KEY=VALUE, получено %q", lineNo, line)
		}
		key = strings.TrimSpace(key)
		value = unquote(strings.TrimSpace(value))
		if key == "" {
			return fmt.Errorf(".env: строка %d: пустое имя переменной", lineNo)
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf(".env: строка %d: %w", lineNo, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf(".env: чтение: %w", err)
	}
	return nil
}

// unquote снимает одну пару парных кавычек ('…' или "…"), если они окружают
// значение целиком. Внутренние escape-последовательности не обрабатываются —
// в этом .env только URL, токены и числа.
func unquote(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
