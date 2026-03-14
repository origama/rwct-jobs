# RWCT Agent Services

Pipeline a microservizi Go: `RSS -> SQLite queue -> LLM analyzer -> dispatcher`.

## Servizi

- `rss-reader`: poll feed RSS, deduplica item e pubblica payload raw in `analyzer_queue`.
- `job-analyzer`: consuma `analyzer_queue`, analizza con LLM locale (llama.cpp), salva payload analizzato e accoda su `dispatch_queue`.
- `message-dispatcher`: consuma `dispatch_queue`, renderizza template markdown e invia a file o Telegram.
- `web-admin`: UI per feed management, monitor code, runtime analyzer, requeue item e ispezione payload analizzati.

## Stato e queue

- Queue raw: `analyzer_queue` (`QUEUED -> LEASED -> DONE`).
- Queue analyzed: `dispatch_queue` (`QUEUED -> LEASED -> DONE`).
- Stato business item: `rss_items.status` (`NEW`, `ANALYZED`, `DISPATCHED`, `FAILED`).
- Le code sono implementate via SQLite condiviso, senza broker esterno.

Per i diagrammi completi delle state machine/lifecycle:
- [Lifecycle e State Machine](docs/lifecycle-state-machines.md)

## Avvio rapido

```bash
cp .env.example .env
cp .env.secrets.example .env.secrets
docker compose --profile dev up --build
```

Output file sink di default:
- `/data/outbox/messages.md` nel volume `rwct-data`.

Profilo produzione (llama.cpp):

```bash
docker compose --profile prod up --build
```

## Configurazione analyzer (chiavi principali)

- `LLM_MODEL`, `LLM_ENDPOINT`, `LLM_TIMEOUT`, `LLM_MAX_TOKENS`
- `LLM_THINKING_ENABLED=true|false`: inoltra `enable_thinking` alla `/v1/chat/completions` quando supportato.
- `ANALYZER_MAX_PARALLEL_JOBS` (default `1`): massimo numero di job in parallelo per singolo processo `job-analyzer` (attualmente forzato a `1` in strict sequential mode).
- `ANALYZER_SCRAPE_SOURCE_PAGE=true|false` (default `true`): fa fetch della pagina linkata nel job e aggiunge il testo estratto al prompt.
- `ANALYZER_MAX_DELIVERY_ATTEMPTS` (default `3`): soglia anti-poison item su `analyzer_queue`.
- `LLM_PARALLEL_THREADS` (default `-1`): numero thread CPU per `llama.cpp` (`-t`, auto-detect quando `-1`).

## Telegram

Per usare Telegram impostare:
- `DESTINATION_MODE=telegram`
- `TELEGRAM_BOT_TOKEN` (in `.env.secrets`)
- `TELEGRAM_CHAT_ID` (in `.env.secrets`)
- `TELEGRAM_TEMPLATE_FILE` (es. `/app/configs/message.telegram.tmpl.md`)

Opzionali:
- `TELEGRAM_THREAD_ID`
- `TELEGRAM_PARSE_MODE`
- `TELEGRAM_DISABLE_WEB_PAGE_PREVIEW`

Template:
- `DISPATCH_TEMPLATE_FILE`: template Go `text/template` usato per destinazioni non-Telegram.
- `TELEGRAM_TEMPLATE_FILE`: template Go `text/template` dedicato a Telegram (fallback su `DISPATCH_TEMPLATE_FILE` se non impostato).

## Web Admin

- URL: `http://localhost:8090` (`WEB_ADMIN_PORT` per override)
- Funzioni principali:
- gestione feed (`add/remove/enable/disable/force poll`)
- monitor queue e pipeline (`raw/analyzed backlog/inflight`, stuck detectors)
- pannello analyzer runtime (modello attivo, thinking, timeout, max tokens, rate)
- requeue di item analizzati e visualizzazione JSON processato

## Troubleshooting

- [Troubleshooting operativo](docs/troubleshooting.md)

## Test

```bash
go test ./...
```
