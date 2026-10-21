// Scope-политика (§9.1): public | chat:<id> | internal.
// Группа ≠ public; DM-факт не переносится в группу. Пересечение разрешений
// производного объекта — пересечение scope'ов его источников; пустое → internal.

export type ScopeId = string;

export const PUBLIC_SCOPE = "public";
export const INTERNAL_SCOPE = "internal";

export function chatScope(chatId: string): ScopeId {
  return `chat:${chatId}`;
}

export function isValidScope(scope: string): scope is ScopeId {
  return scope === PUBLIC_SCOPE || scope === INTERNAL_SCOPE || /^chat:-?\d+$/.test(scope);
}

/**
 * Пересечение аудиторий (§9.1): производный объект доступен там, где
 * доступны ВСЕ его источники. У двух разных приватных аудиторий общего
 * чата может не быть — тогда internal.
 */
export function intersectScopes(scopes: ScopeId[]): ScopeId {
  const unique = [...new Set(scopes)];
  if (unique.length === 0) return INTERNAL_SCOPE;
  if (unique.length === 1) return unique[0]!;
  // internal никогда не расширяет доступ
  if (unique.includes(INTERNAL_SCOPE)) return INTERNAL_SCOPE;
  // public не сужает — пересечение остальных
  const meaningful = unique.filter((scope) => scope !== PUBLIC_SCOPE);
  if (meaningful.length === 0) return PUBLIC_SCOPE;
  if (meaningful.length === 1) return meaningful[0]!;
  return INTERNAL_SCOPE;
}

/** Виден ли объект из scope-аудитории. */
export function visibleFrom(objectScope: ScopeId, audienceScope: ScopeId): boolean {
  return objectScope === PUBLIC_SCOPE || objectScope === audienceScope;
}
