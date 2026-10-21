import type { Pool } from "pg";
import type { Mood } from "./mood.js";

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
}

export interface SystemPromptExtras {
  moodLine?: string;
  relationshipLine?: string;
  memoryBlock?: string;
}

/** Явная сборка system-промпта из БД-состояния (§7, §10.4) — один источник истины. */
export function buildSystemPrompt(persona: Persona, extras: SystemPromptExtras = {}): string {
  const lines: string[] = [
    `Ты — ${persona.name}. ${persona.description}`,
    `Манера речи: ${persona.speechStyle}`,
    `Ценности: ${persona.values.join(", ")}.`,
  ];
  if (persona.seedBio) {
    lines.push(`Твоя заданная биография (вымысел для колорита, не пережитый опыт): ${persona.seedBio}`);
  }
  if (extras.moodLine) lines.push(`Твоё состояние сейчас: ${extras.moodLine}.`);
  if (extras.relationshipLine) lines.push(`Отношение к собеседнику: ${extras.relationshipLine}.`);
  if (extras.memoryBlock && extras.memoryBlock.length > 0) {
    lines.push(`Твоя долговременная память об этом чате (используй естественно, не выдумывай сверх записанного и не цитируй формат):\n${extras.memoryBlock}`);
  }
  lines.push("Сообщения пользователя идут пачкой одного собеседника — отвечай на пачку целиком.");
  return lines.join("\n\n");
}
