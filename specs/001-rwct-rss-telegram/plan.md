# Implementation Plan: RWCT Job Feeds Agent (MVP)

**Branch**: `001-rwct-rss-telegram` | **Date**: 2026-03-04 | **Spec**: [/Users/gvirzi/Library/CloudStorage/SynologyDrive-NASSynced/Akamai/repos/origama/rwct-agent/specs/001-rwct-rss-telegram/spec.md](/Users/gvirzi/Library/CloudStorage/SynologyDrive-NASSynced/Akamai/repos/origama/rwct-agent/specs/001-rwct-rss-telegram/spec.md)
**Input**: Feature specification from `/specs/001-rwct-rss-telegram/spec.md`

## Summary

Implementare un sistema a 3 microservizi Go (`rss-reader`, `job-analyzer`, `message-dispatcher`) orchestrati tramite code SQLite persistenti (`analyzer_queue`, `dispatch_queue`), con dedup su SQLite, analisi LLM self-hosted CPU-only (llama.cpp compatibile), output Telegram/file e template RWCT-JOBS che includa link e immagini estratti dall'item RSS originale.

## Technical Context

**Language/Version**: Go 1.24  
**Primary Dependencies**: gofeed, SQLite (modernc driver), OpenTelemetry SDK metric (OTLP gRPC), Go `text/template`  
**Storage**: SQLite condiviso (stato feed, dedup, queue leasing/ack, audit minimi)  
**Testing**: `go test` (unit + integration + e2e)  
**Target Platform**: Linux containers (Docker Compose)  
**Project Type**: Backend microservices (event-driven con DB queue)  
**Performance Goals**: 5-100 job/day, end-to-end <= 5 minuti per item in condizioni nominali  
**Constraints**: CPU-only, no GPU, max concurrency analyzer=1 per istanza, no dipendenza da broker esterno  
**Scale/Scope**: 1 maintainer, 3 servizi, 1 DB SQLite condiviso

## Constitution Check

- Sviluppo spec-driven rispettato (`spec.md` + `plan.md` prima di `tasks.md`).
- Qualità: test richiesti inclusi nello scope (unit/integration/e2e).
- Osservabilità: healthcheck + logging strutturato + endpoint OTel configurabile via env.
- Nessuna violazione bloccante identificata per MVP.

## Project Structure

### Documentation (this feature)

```text
specs/001-rwct-rss-telegram/
├── plan.md
└── spec.md
```

### Source Code (repository root)

```text
cmd/
├── job-analyzer/
├── llm-mock/
├── message-dispatcher/
└── rss-reader/

internal/
├── analyzer/
├── dispatcher/
├── rssreader/
└── webadmin/

pkg/
├── events/
├── health/
├── retry/
└── telemetry/

configs/
├── feeds.txt
└── message.tmpl.md

tests/
├── e2e/
└── integration/

Dockerfile
docker-compose.yml
.env.example
README.md
```

**Structure Decision**: Monorepo Go con 3 binari indipendenti, pacchetti condivisi per contratti/eventi e utility. Compose gestisce runtime locale completo con profili differenti (`dev` con mock LLM, `prod` con llama.cpp server), senza broker esterno.

## Queue Model (SQLite)

- `analyzer_queue`: job raw (`QUEUED` -> `LEASED` -> `DONE`) consumati da `job-analyzer`.
- `dispatch_queue`: job analizzati (`QUEUED` -> `LEASED` -> `DONE`) consumati da `message-dispatcher`.
- `feed_poll_requests`: richieste force-poll feed consumate da `rss-reader`.

Semantics:

- Claim atomico con lease (`lease_owner`, `lease_until`) e counter (`delivery_count`).
- Release su errore (`state=QUEUED`, `last_error`) per retry controllato.
- Complete su successo (`state=DONE`).
- Idempotenza applicativa su `item_id` e stato `rss_items`.

Payload policy:

- JSON versionato con `schema_version = "v1"`.
- Payload analizzato include `source_label`, `original_links[]`, `original_images[]`.

## Delivery/Retry Policy

- Retry applicativo analyzer/dispatcher: 3 tentativi, backoff esponenziale (base 2s), con release claim su errore.
- RSS feed retry: 5 tentativi, backoff esponenziale; poi feed `down`.
- Feed down handling: cooldown retry periodico (default 6h), ma force-poll esplicito da web-admin bypassa il cooldown.

## Runtime Profiles

- `dev`: 3 servizi + `llm-mock` (nessun modello richiesto).
- `prod`: 3 servizi + `llama-cpp` (modello GGUF montato da volume).

## Implementation Phases

1. Core scaffold: repo layout, config, docker, healthcheck, contracts.
2. RSS ingestion: feed polling, dedup SQLite, enqueue raw + force-poll requests.
3. Analyzer: consume `analyzer_queue`, LLM parse, enqueue `dispatch_queue`.
4. Dispatcher: consume `dispatch_queue`, markdown template, sink + rate-limit.
5. Test layer: unit + queue semantics/requeue + e2e pipeline.
6. Hardening: retention cleanup, logging consistency, docs quickstart.

## Next Implementation Backlog

1. Centralizzare schema/migrazioni SQLite e DSN condiviso tra `rss-reader`, `job-analyzer`, `message-dispatcher`, `web-admin`.
2. Proteggere `web-admin` per utilizzo non locale con auth minima o esposizione limitata/documentata.
3. Rifattorizzare i file monolitici dei servizi, in particolare `internal/webadmin/service.go`, separando HTTP, SQL e UI assets.
4. Estrarre bootstrap e validazione config/env in un package condiviso, eliminando `MustEnv*` con `panic`.
5. Uniformare lifecycle operativo: health server chiudibile, telemetry dev/prod e shutdown coerente.
6. Rendere esplicita e supportata la configurazione della modalita` `thinking` del modello nell'analyzer.
7. Ridurre query N+1 nel `web-admin` usando query aggregate.
8. Allineare le capability configurabili alle feature effettivamente implementate, in particolare sulla concorrenza analyzer.
9. Introdurre fallback opzionale di scraping con browser headless (feature flag) per pagine protette da anti-bot/JS challenge, con rate limit basso, cache e degradazione controllata a contenuto RSS-only.
10. Implementare nel `web-admin` una webview multi-scheda con viste operative per item per feed, item per queue e item per stato.

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| Nessuna | N/A | N/A |
