import { describe, expect, it } from "vitest";
import fc from "fast-check";
import { chatScope, intersectScopes, visibleFrom } from "./scope.js";

// PBT scope-политики (§9.1). Пересечение аудиторий производного объекта —
// это пересечение разрешений его источников: оно НЕ РАСШИРЯЕТ доступ ни
// сверх одного из участников (звук) и НЕ СУЖИТ сверх общего ядра (полнота).
// «Самый узкий» non-public участник определяет потолок видимости.

const inputChatIdArb = fc.integer({ min: -1000, max: 1000 });
const scopeArb = fc.oneof(
  fc.constant("public" as const),
  fc.constant("internal" as const),
  inputChatIdArb.map((id) => chatScope(String(id))),
);
const scopesArb = fc.array(scopeArb, { maxLength: 6 });
/** Пустой набор источников — отдельный случай (missing provenance → internal, §9.1),
 *  и свойство полноты к нему неприменимо (vacuous truth). */
const nonEmptyScopesArb = fc.array(scopeArb, { minLength: 1, maxLength: 6 });

/** Свежий chat-scope, гарантированно не совпадающий ни с одним входным. */
const freshChatScopeArb = fc
  .integer({ min: 1_000_000, max: 2_000_000 })
  .map((id) => chatScope(String(id)));

/** Все значимые аудитории проверки: каждый участник, public, internal, чужой чат. */
function audiencesOf(scopes: string[], freshScope: string): string[] {
  return [...new Set([...scopes, "public", "internal", freshScope])];
}

/**
 * Аудитории для свойства ЗВУКА (пересечение не расширяет доступ): чаты и public.
 * internal исключена сознательно: §9.1 объявляет internal фолбэком для пустого
 * пересечения («результат становится internal»), т.е. internal-объект по
 * определению виден внутренней аудитории, даже если ни один chat-источник из
 * internal не виден. Это спецификационная асимметрия, не дыра реализации
 * (см. отдельный пример ниже и находки отчёта).
 */
function chatAudiencesOf(scopes: string[], freshScope: string): string[] {
  return [...new Set([...scopes.filter((scope) => scope !== "internal"), "public", freshScope])];
}

describe("PBT: intersectScopes — пересечение никогда не расширяет доступ (§9.1)", () => {
  it("если результат виден аудитории, то виден КАЖДЫЙ участник пересечения", async () => {
    await fc.assert(
      fc.property(scopesArb, freshChatScopeArb, (scopes, freshScope) => {
        const result = intersectScopes(scopes);
        for (const audience of chatAudiencesOf(scopes, freshScope)) {
          if (visibleFrom(result, audience)) {
            for (const member of scopes) {
              expect(
                visibleFrom(member, audience),
                `результат ${result} виден из ${audience}, но участник ${member} — нет`,
              ).toBe(true);
            }
          }
        }
      }),
    );
  });

  it("спецификационный фолбэк §9.1: пустое пересечение → internal, видимый внутренней аудитории", () => {
    // асимметрия документирована: участник chat:1 из internal НЕ виден
    // (visibleFrom("chat:1","internal") === false), но результат-фолбэк
    // internal виден внутренней аудитории — так велит §9.1
    expect(intersectScopes(["chat:1", "chat:2"])).toBe("internal");
    expect(visibleFrom("internal", "internal")).toBe(true);
    expect(visibleFrom("chat:1", "internal")).toBe(false);
  });

  it("среди участников есть non-public → результат не public (потолок самого узкого)", async () => {
    await fc.assert(
      fc.property(scopesArb, (scopes) => {
        const hasNonPublic = scopes.some((scope) => scope !== "public");
        const result = intersectScopes(scopes);
        expect(hasNonPublic && result === "public").toBe(false);
      }),
    );
  });

  it("результат — public/internal либо один из входных chat-scope: новых аудиторий не возникает", async () => {
    await fc.assert(
      fc.property(scopesArb, (scopes) => {
        const result = intersectScopes(scopes);
        const known = new Set<string>([...scopes, "public", "internal"]);
        expect(known.has(result)).toBe(true);
      }),
    );
  });
});

describe("PBT: intersectScopes — пересечение не сужает сверх общего ядра (§9.1)", () => {
  it("аудитория, видящая ВСЕХ участников, видит и результат", async () => {
    await fc.assert(
      fc.property(nonEmptyScopesArb, freshChatScopeArb, (scopes, freshScope) => {
        const result = intersectScopes(scopes);
        for (const audience of audiencesOf(scopes, freshScope)) {
          if (scopes.every((member) => visibleFrom(member, audience))) {
            expect(
              visibleFrom(result, audience),
              `все участники видны из ${audience}, а результат ${result} — нет`,
            ).toBe(true);
          }
        }
      }),
    );
  });

  it("две разные приватные аудитории без общего чата — internal (текст §9.1)", () => {
    expect(intersectScopes(["chat:1", "chat:2"])).toBe("internal");
    expect(intersectScopes(["chat:1", "chat:2", "public"])).toBe("internal");
    expect(intersectScopes(["chat:-100", "chat:7", "public", "chat:7"])).toBe("internal");
  });
});

describe("PBT: intersectScopes — устойчивость к порядку и дублям источников", () => {
  it("перестановка участников не меняет пересечение", async () => {
    await fc.assert(
      fc.property(scopeArb, scopeArb, (first, second) => {
        expect(intersectScopes([first, second])).toBe(intersectScopes([second, first]));
      }),
    );
  });

  it("дубликаты источников не меняют пересечение", async () => {
    await fc.assert(
      fc.property(scopesArb, (scopes) => {
        expect(intersectScopes([...scopes, ...scopes])).toBe(intersectScopes(scopes));
      }),
    );
  });
});

describe("PBT: согласованность intersectScopes и visibleFrom", () => {
  it("пересечение одного участника — сам участник; доступ к нему совпадает", async () => {
    await fc.assert(
      fc.property(scopeArb, freshChatScopeArb, (scope, freshScope) => {
        expect(intersectScopes([scope])).toBe(scope);
        for (const audience of ["public", "internal", scope, freshScope]) {
          expect(visibleFrom(intersectScopes([scope]), audience)).toBe(
            visibleFrom(scope, audience),
          );
        }
      }),
    );
  });

  it("chat-scope никогда не виден из чужого чата, как бы его ни пересекали", async () => {
    await fc.assert(
      fc.property(
        scopesArb.filter((scopes) => scopes.some((scope) => scope.startsWith("chat:"))),
        freshChatScopeArb,
        (scopes, freshScope) => {
          expect(visibleFrom(intersectScopes(scopes), freshScope)).toBe(false);
        },
      ),
    );
  });
});
