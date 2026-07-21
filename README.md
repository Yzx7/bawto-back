# sacs-chatbots — backend

Backend de la plataforma de chatbots multicanal (WhatsApp-first) de **Sistemuino S.A.C.S**.
Go + Fiber + PostgreSQL (pgx). Ver el diseño completo en [`../ARQUITECTURA.md`](../ARQUITECTURA.md).

## Requisitos
- Go 1.25+
- PostgreSQL 14+ (con extensiones `pgcrypto` y `pgvector`)

## Setup

```bash
cp .env.example .env        # completar DATABASE_URL, TOKEN_ENC_KEY, ANTHROPIC_API_KEY
```

Crear la base y aplicar el schema:

```bash
psql "$DATABASE_URL" -f db/schema.sql
```

## Correr

```bash
go run .
```

Health check: `GET http://localhost:3009/` → `{ "ok": true, ... }`

## Estructura

```
bootstrap/   wiring del runtime (config, pg pool, fiber, rutas)
config/      carga de configuración (env)
env/         contenedor de dependencias (env.Env)
db/          conexión Postgres + schema.sql
routes/      declaración de endpoints + middlewares
middlewares/ auth (sesión cookie) y demás
controllers/ capa HTTP por dominio
models/      SQL puro
channels/    abstracción de canal + adapter WhatsApp
engine/      motor de capas + IA (Claude, RAG)
queue/       cola de webhooks (idempotencia)
types/       GenRes
```

## Estado
Fase 0 (scaffold): servidor arranca, conecta a Postgres, health check y rutas base.
Siguiente: Fase 1 (canal WhatsApp) y Fase 2 (IA).
