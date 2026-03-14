# RWCT Agent Services

Pipeline a microservizi Go: `RSS -> SQLite queue -> LLM analyzer -> dispatcher`.

## Servizi

- `rss-reader`: poll feed RSS, deduplica item e pubblica payload raw in `analyzer_queue`.
- `job-analyzer`: consuma `analyzer_queue`, analizza con LLM locale (llama.cpp), salva payload analizzato e accoda su `dispatch_queue`.
- `scrapling-sidecar`: endpoint HTTP (`/extract`) per source-enrichment avanzato delle pagine remote quando `ANALYZER_SOURCE_EXTRACTOR` usa `scrapling`/`hybrid`.
- `message-dispatcher`: consuma `dispatch_queue`, renderizza template markdown e invia a file o Telegram.
- `web-admin`: UI per feed management, monitor code, runtime analyzer, requeue item e ispezione payload analizzati.
- `test-rss`: mini-sito di test che espone feed RSS (`/feed.rss`) e pagine annuncio locali (`/jobs/...`) generate a intervallo configurabile.
- `otel-collector`: riceve metriche/log/tracce via OTLP da tutti i componenti e inoltra verso backend osservabilità.
- `prometheus`, `loki`, `tempo`: backend locali per metriche, log e tracce.
- `grafana`: dashboard e correlazione tra metriche/log/tracce.

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

`otel-collector` viene avviato sempre (anche senza `obs`) e inoltra i segnali OTLP a Grafana Cloud.

Stack observability locale opzionale (profilo separato):

```bash
docker compose --profile obs up -d
```

Per avviare tutto insieme (`prod` + observability):

```bash
docker compose --profile prod --profile obs up --build
```

Endpoint observability locale (solo profilo `obs`):
- Grafana: `http://localhost:${GRAFANA_PORT:-3000}` (default `admin/admin`)
- Prometheus: `http://localhost:9090`
- Loki: `http://localhost:3100`
- Tempo: `http://localhost:3200`

## Configurazione analyzer (chiavi principali)

- `LLM_MODEL`, `LLM_ENDPOINT`, `LLM_TIMEOUT`, `LLM_MAX_TOKENS`
- `LLM_TIMEOUT_MAX`: timeout massimo hard per richiesta LLM.
- `LLM_TIMEOUT_PER_1K_CHARS`: budget extra per prompt lunghi (ogni ~1000 caratteri).
- `LLM_TIMEOUT_PER_256_TOKENS`: budget extra per risposte lunghe (ogni 256 `max_tokens` richiesti).
- `LLM_THINKING_ENABLED=true|false`: inoltra `enable_thinking` alla `/v1/chat/completions` quando supportato.
- `ANALYZER_PROMPT_TEMPLATE`: prompt principale custom (Go `text/template`) con variabili `AllowedCategories`, `Title`, `URL`, `Description`, `SourcePageText`, `LinksCSV`, `ImagesCSV`.
- `ANALYZER_COMPACT_PROMPT_TEMPLATE`: prompt fallback compatto custom con le stesse variabili.
- `ANALYZER_MAX_PARALLEL_JOBS` (default `1`): massimo numero di job in parallelo per singolo processo `job-analyzer` (attualmente forzato a `1` in strict sequential mode).
- `ANALYZER_SOURCE_EXTRACTOR=off|basic|scrapling|hybrid` (default `basic`): strategia di source-enrichment della pagina linkata nel job.
- `ANALYZER_SOURCE_MIN_CHARS_FOR_BASIC` (default `220`): soglia minima testo per attivare fallback in `hybrid`.
- `ANALYZER_SCRAPLING_ENDPOINT` (default `http://scrapling-sidecar:8088`): endpoint sidecar Scrapling.
- `ANALYZER_SCRAPLING_TIMEOUT` (default `8s`): timeout chiamata Scrapling.
- `ANALYZER_SCRAPLING_MAX_CHARS` (default `10000`): clamp testo estratto da Scrapling.
- `ANALYZER_MAX_DELIVERY_ATTEMPTS` (default `3`): soglia anti-poison item su `analyzer_queue`.
- `LLM_PARALLEL_THREADS` (default `-1`): numero thread CPU per `llama.cpp` (`-t`, auto-detect quando `-1`).

## OpenTelemetry / Observability

Tutti i componenti (`rss-reader`, `job-analyzer`, `message-dispatcher`, `web-admin`, `test-rss`, `llm-mock`, `scrapling-sidecar`) sono instrumentati con OpenTelemetry SDK e inviano segnali all'OTel Collector locale.

Variabili principali:
- `OTEL_EXPORTER_OTLP_ENDPOINT` (default `otel-collector:4317`)
- `OTEL_EXPORTER_OTLP_INSECURE` (default `true`)
- `SERVICE_VERSION` (default `dev`)
- `DEPLOY_ENV` (default `local`)

Forwarding Grafana Cloud (collector):
- endpoint OTLP: `https://otlp-gateway-prod-eu-west-0.grafana.net/otlp`
- credenziali in `.env.secrets`:
  - `GRAFANA_INSTANCE_ID`
  - `GRAFANA_TOKEN`

Dashboard provisionata automaticamente:
- `RWCT Observability Overview` (Grafana folder `RWCT`)
- file: `observability/grafana/dashboards/rwct-overview.json`

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
- [Osservability stack](docs/observability-stack.md)

## Test

```bash
go test ./...
```
