import { jsonrepair } from "jsonrepair";

// Модель иногда отдаёт почти-JSON в аргументах tool-call: двойное двоеточие
// после ключа, оборванный массив, висячая запятая. Строгий JSON.parse на
// таком падает, и ход уходит в повторный вызов — дорого и медленно.
// Здесь чиним до валидации: структуру всё равно проверит TurnResultSchema.

/** Двойное двоеточие после ключа: {"mood_delta":: ...} → {"mood_delta": ...} */
function fixDoubleColon(raw: string): string {
  return raw.replace(/"\s*::/g, '":');
}

/**
 * Разбирает аргументы tool-call, пробуя по очереди:
 * как есть → с исправленным двоеточием → jsonrepair на обоих вариантах.
 * Бросает, если даже jsonrepair не справился (тогда адаптер повторит вызов).
 */
export function parseToolArguments(raw: string): unknown {
  const candidates = [raw, fixDoubleColon(raw)];
  for (const candidate of candidates) {
    for (const attempt of [candidate, safeRepair(candidate)]) {
      if (attempt === null) continue;
      try {
        const parsed: unknown = JSON.parse(attempt);
        // Аргументы функции — всегда объект; строка/массив — уже не наш случай.
        if (typeof parsed === "object" && parsed !== null && !Array.isArray(parsed)) {
          return parsed;
        }
      } catch {
        // следующий вариант
      }
    }
  }
  throw new Error("аргументы tool-call не являются JSON-объектом даже после ремонта");
}

function safeRepair(raw: string): string | null {
  try {
    return jsonrepair(raw);
  } catch {
    return null;
  }
}
