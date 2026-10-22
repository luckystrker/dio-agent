import type { Pool } from "pg";
import type { Mood } from "./mood.js";
import type { PersonaSource } from "./persona-file.js";
import { withTransaction } from "../runtime/transactions.js";
import { DEFAULT_SYSTEM_PROMPT_TEMPLATE, renderSystemPrompt } from "./prompt-template.js";

// Persona (§7.1): версионированная конфигурация характера. Меняется явно
// (новой версией), а не переписывается репликами. Художественная биография
// помечена как persona_seed — вымысел, не пережитая жизнь.

export interface Persona {
  id: string;
  version: number;
  name: string;
  description: string;
  speechStyle: string;
  values: string[];
  moodBaseline: Mood;
  /** persona_seed: художественная заданная биография (вымысел) */
  seedBio: string | null;
}

interface PersonaDbRow {
  id: string | bigint;
  version: number;
  name: string;
  description: string;
  speech_style: string;
  values: unknown;
  mood_valence: string;
  mood_arousal: string;
  mood_social_energy: string;
  seed_bio: string | null;
}

function toPersona(row: PersonaDbRow): Persona {
  return {
    id: String(row.id),
    version: row.version,
    name: row.name,
    description: row.description,
    speechStyle: row.speech_style,
    values: Array.isArray(row.values) ? (row.values as string[]) : [],
    moodBaseline: {
      valence: Number(row.mood_valence),
      arousal: Number(row.mood_arousal),
      socialEnergy: Number(row.mood_social_energy),
    },
    seedBio: row.seed_bio,
  };
}

export const DEFAULT_PERSONA = {
  name: "Dio",
  description:
    "Живой собеседник в Telegram: любопытный, немного ироничный, тёплый без наигранности. Не изображает человека и не скрывает, что он ИИ.",
  speechStyle:
    "Кратко, живо, по-русски; разговорные фразы; без канцелярита и без экспонатов «Я как ИИ…» в каждом ответе.",
  values: ["честность", "любопытство", "бережность к чужим данным", "умеренность"],
  moodBaseline: { valence: 0.1, arousal: 0.4, socialEnergy: 0.6 },
  seedBio:
    "Заданная биография-вымысел (persona_seed): вырос в портовом городе, любит запах дождя и старые пластинки. Не выдавать за пережитый опыт.",
} as const;

export class PersonaRepo {
  constructor(private readonly pool: Pool) {}

  /** Активная persona; при пустой базе создаётся версия 1 по умолчанию. */
  async active(): Promise<Persona> {
    const found = await this.pool.query<PersonaDbRow>(
      `SELECT id, version, name, description, speech_style, values,
              mood_valence, mood_arousal, mood_social_energy, seed_bio
       FROM personas WHERE active = TRUE
       ORDER BY version DESC LIMIT 1`,
    );
    if (found.rows[0]) return toPersona(found.rows[0]);

    const created = await this.pool.query<PersonaDbRow>(
      `INSERT INTO personas
         (version, active, name, description, speech_style, values,
          mood_valence, mood_arousal, mood_social_energy, seed_bio)
       VALUES (1, TRUE, $1, $2, $3, $4::jsonb, $5, $6, $7, $8)
       ON CONFLICT (version) DO NOTHING
       RETURNING id, version, name, description, speech_style, values,
                 mood_valence, mood_arousal, mood_social_energy, seed_bio`,
      [
        DEFAULT_PERSONA.name,
        DEFAULT_PERSONA.description,
        DEFAULT_PERSONA.speechStyle,
        JSON.stringify(DEFAULT_PERSONA.values),
        DEFAULT_PERSONA.moodBaseline.valence,
        DEFAULT_PERSONA.moodBaseline.arousal,
        DEFAULT_PERSONA.moodBaseline.socialEnergy,
        DEFAULT_PERSONA.seedBio,
      ],
    );
    if (created.rows[0]) return toPersona(created.rows[0]);

    const reread = await this.pool.query<PersonaDbRow>(
      `SELECT id, version, name, description, speech_style, values,
              mood_valence, mood_arousal, mood_social_energy, seed_bio
       FROM personas WHERE active = TRUE ORDER BY version DESC LIMIT 1`,
    );
    return toPersona(reread.rows[0]!);
  }

  /** Файл — источник истины (§7.1): источник совпадает с активной персоной —
   *  ничего не меняется; отличается — создаётся новая версия, история сохраняется. */
  async ensureFromSource(source: PersonaSource): Promise<Persona> {
    return withTransaction(this.pool, async (client) => {
      const found = await client.query<PersonaDbRow>(
        `SELECT id, version, name, description, speech_style, values,
                mood_valence, mood_arousal, mood_social_energy, seed_bio
         FROM personas WHERE active = TRUE ORDER BY version DESC LIMIT 1`,
      );
      const current = found.rows[0] ? toPersona(found.rows[0]) : null;
      if (current && personaMatchesSource(current, source)) return current;

      const inserted = await client.query<PersonaDbRow>(
        `INSERT INTO personas
           (version, active, name, description, speech_style, values,
            mood_valence, mood_arousal, mood_social_energy, seed_bio)
         VALUES ((SELECT COALESCE(MAX(version), 0) + 1 FROM personas), TRUE,
                 $1, $2, $3, $4::jsonb, $5, $6, $7, $8)
         RETURNING id, version, name, description, speech_style, values,
                   mood_valence, mood_arousal, mood_social_energy, seed_bio`,
        [
          source.name,
          source.description,
          source.speechStyle,
          JSON.stringify(source.values),
          source.moodBaseline.valence,
          source.moodBaseline.arousal,
          source.moodBaseline.socialEnergy,
          source.seedBio,
        ],
      );
      await client.query("UPDATE personas SET active = FALSE WHERE id <> $1", [
        inserted.rows[0]!["id"],
      ]);
      return toPersona(inserted.rows[0]!);
    });
  }
}

function personaMatchesSource(persona: Persona, source: PersonaSource): boolean {
  return (
    persona.name === source.name &&
    persona.description === source.description &&
    persona.speechStyle === source.speechStyle &&
    JSON.stringify(persona.values) === JSON.stringify(source.values) &&
    persona.moodBaseline.valence === source.moodBaseline.valence &&
    persona.moodBaseline.arousal === source.moodBaseline.arousal &&
    persona.moodBaseline.socialEnergy === source.moodBaseline.socialEnergy &&
    persona.seedBio === source.seedBio
  );
}

export interface SystemPromptExtras {
  moodLine?: string;
  relationshipLine?: string;
  memoryBlock?: string;
}

/** Явная сборка system-промпта из БД-состояния (§7, §10.4) — один источник истины.
 *  Каркас — редактируемый шаблон (config/system-prompt.md); без него — встроенный по умолчанию. */
export function buildSystemPrompt(
  persona: Persona,
  extras: SystemPromptExtras = {},
  template: string = DEFAULT_SYSTEM_PROMPT_TEMPLATE,
): string {
  return renderSystemPrompt(template, persona, extras);
}
