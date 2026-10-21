// Кандидаты памяти из TurnResult (§10.1): модель ПРЕДЛАГАЕТ, runtime
// проверяет (типы, длины, лимиты, допустимые значения). Источники модель
// не назначает вовсе — runtime присваивает сам (§8.3, §17), поэтому
// «сослаться на несуществующий источник» невозможно по построению.

import { ENTITY_KINDS, type EntityKind } from "./entities.js";

export const MEMORY_KINDS = ["claim", "experience", "thought", "preference"] as const;
export type MemoryKind = (typeof MEMORY_KINDS)[number];

export type EntityRole = "subject" | "participant" | "place" | "context";
const ENTITY_ROLES: readonly EntityRole[] = ["subject", "participant", "place", "context"];

/** Начальные пределы §10.1: до 5 кандидатов памяти на ход. */
export const MAX_MEMORY_CANDIDATES = 5;
export const MAX_ENTITIES_PER_CANDIDATE = 3;
const MAX_TEXT_LENGTH = 2000;
const MAX_NAME_LENGTH = 200;

export interface CandidateEntity {
  name: string;
  kind: EntityKind;
  role: EntityRole;
}

export interface MemoryCandidate {
  kind: MemoryKind;
  text: string;
  entities: CandidateEntity[];
  /** 0..1, значимость для будущего вспоминания, не истинность */
  salience?: number;
  /** 0..1, только для проверяемых claim */
  confidence?: number;
}

export class CandidateError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CandidateError";
  }
}

/**
 * Чистая валидация предложений модели. Всё, что не прошло, — отклоняется
 * целиком ( кандидат с ошибкой не «лучшее усилие»), исходное событие
 * остаётся в журнале (§10.1).
 */
export function validateMemoryCandidates(input: unknown): MemoryCandidate[] {
  if (!Array.isArray(input)) {
    throw new CandidateError("memory должен быть массивом");
  }
  if (input.length > MAX_MEMORY_CANDIDATES) {
    throw new CandidateError(`больше ${MAX_MEMORY_CANDIDATES} кандидатов памяти за ход`);
  }

  return input.map((entry, index) => {
    const prefix = `кандидат #${index}`;
    if (typeof entry !== "object" || entry === null) {
      throw new CandidateError(`${prefix}: ожидается объект`);
    }
    const record = entry as Record<string, unknown>;

    const kind = record["kind"];
    if (typeof kind !== "string" || !(MEMORY_KINDS as readonly string[]).includes(kind)) {
      throw new CandidateError(`${prefix}: неизвестный вид "${String(kind)}"`);
    }

    const text = record["text"];
    if (typeof text !== "string" || text.trim().length === 0) {
      throw new CandidateError(`${prefix}: text должен быть непустой строкой`);
    }
    if (text.length > MAX_TEXT_LENGTH) {
      throw new CandidateError(`${prefix}: text длиннее ${MAX_TEXT_LENGTH} символов`);
    }

    const entitiesRaw = record["entities"] ?? [];
    if (!Array.isArray(entitiesRaw) || entitiesRaw.length > MAX_ENTITIES_PER_CANDIDATE) {
      throw new CandidateError(`${prefix}: entities — массив до ${MAX_ENTITIES_PER_CANDIDATE}`);
    }
    const entities: CandidateEntity[] = entitiesRaw.map((raw, entityIndex) => {
      const entityPrefix = `${prefix}, сущность ${entityIndex}`;
      if (typeof raw !== "object" || raw === null) {
        throw new CandidateError(`${entityPrefix}: ожидается объект`);
      }
      const entity = raw as Record<string, unknown>;
      const name = entity["name"];
      if (typeof name !== "string" || name.trim().length === 0 || name.length > MAX_NAME_LENGTH) {
        throw new CandidateError(`${entityPrefix}: name 1..${MAX_NAME_LENGTH} символов`);
      }
      const entityKind = entity["kind"] ?? "topic";
      if (typeof entityKind !== "string" || !(ENTITY_KINDS as readonly string[]).includes(entityKind)) {
        throw new CandidateError(`${entityPrefix}: неизвестный kind сущности`);
      }
      const role = entity["role"] ?? "subject";
      if (typeof role !== "string" || !(ENTITY_ROLES as readonly string[]).includes(role)) {
        throw new CandidateError(`${entityPrefix}: неизвестная роль "${String(role)}"`);
      }
      return {
        name: name.trim(),
        kind: entityKind as EntityKind,
        role: role as EntityRole,
      };
    });

    const salience = record["salience"] ?? 0.5;
    if (typeof salience !== "number" || salience < 0 || salience > 1) {
      throw new CandidateError(`${prefix}: salience должен быть числом 0..1`);
    }

    const confidence = record["confidence"];
    if (confidence !== undefined) {
      if (typeof confidence !== "number" || confidence < 0 || confidence > 1) {
        throw new CandidateError(`${prefix}: confidence должен быть числом 0..1`);
      }
      // confidence — только для проверяемых утверждений (§8.2)
      if (kind !== "claim") {
        throw new CandidateError(`${prefix}: confidence допустим только у claim`);
      }
    }

    return {
      kind: kind as MemoryKind,
      text: text.trim(),
      entities,
      salience,
      ...(confidence !== undefined ? { confidence } : {}),
    };
  });
}
