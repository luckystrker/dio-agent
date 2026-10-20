import type { PoolClient } from "pg";

// Закрытый набор дельт этапа 1: append_note (заметка о состоянии).
// Полный набор (mood, relationship, memory, intentions) — этапы 3–4;
// механизм применения «всё или ничего» не изменится.

export interface AppendNoteDelta {
  type: "append_note";
  text: string;
}

export type RuntimeDelta = AppendNoteDelta;

export class DeltaError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "DeltaError";
  }
}

const MAX_TEXT_LENGTH = 2000;

export function validateDeltas(input: unknown): RuntimeDelta[] {
  if (!Array.isArray(input)) {
    throw new DeltaError("дельты должны быть массивом");
  }
  return input.map((entry, index) => {
    if (typeof entry !== "object" || entry === null || !("type" in entry)) {
      throw new DeltaError(`дельта #${index}: ожидается объект с полем type`);
    }
    const record = entry as Record<string, unknown>;
    if (record["type"] !== "append_note") {
      throw new DeltaError(`дельта #${index}: неизвестный тип "${String(record["type"])}"`);
    }
    const text = record["text"];
    if (typeof text !== "string" || text.length === 0) {
      throw new DeltaError(`дельта #${index}: text должен быть непустой строкой`);
    }
    if (text.length > MAX_TEXT_LENGTH) {
      throw new DeltaError(`дельта #${index}: text длиннее ${MAX_TEXT_LENGTH} символов`);
    }
    return { type: "append_note", text } satisfies AppendNoteDelta;
  });
}

/**
 * Применяет дельты ВНУТРИ переданной транзакции. Вызывающий оборачивает в
 * withTransaction — тогда список применяется целиком либо не применяется
 * вовсе (инвариант 4: один ход применяется к состоянию не более одного раза
 * и «всё или ничего»).
 */
export async function applyDeltas(
  client: PoolClient,
  workItemId: string | null,
  deltas: RuntimeDelta[],
): Promise<number> {
  for (const delta of deltas) {
    await client.query(
      "INSERT INTO state_notes (work_item_id, text) VALUES ($1, $2)",
      [workItemId === null ? null : BigInt(workItemId), delta.text],
    );
  }
  return deltas.length;
}
