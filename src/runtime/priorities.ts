// Приоритеты когнитивного worker (ARCHITECTURE §5.2).
// Больше число = выше приоритет. Внутри одного приоритета — FIFO.
export const WORK_PRIORITIES = {
  /** Прямое сообщение, reply, mention */
  direct: 50,
  /** Значимое групповое обсуждение */
  group: 40,
  /** Готовое намерение ответить позже */
  intention: 30,
  /** Исследование */
  exploration: 20,
  /** Консолидация и summary */
  consolidation: 10,
} as const;

export type WorkKind = keyof typeof WORK_PRIORITIES;

export function isWorkKind(value: string): value is WorkKind {
  return Object.hasOwn(WORK_PRIORITIES, value);
}

// Сколько попыток у работы, прежде чем она честно помечается
// терминальным пропуском (skipped), а не висит вечно.
export const WORK_MAX_ATTEMPTS = 3;
