# Features By Component

## rss-reader

### Attuali
- Polling periodico feed RSS configurabile via env.
- Deduplica item su `guid`, URL e titolo normalizzato.
- Estrazione link e immagini dall'item feed.
- Persistenza item raw in SQLite (`rss_items`).
- Enqueue su `analyzer_queue` con lease/retry semantics.
- Backoff su feed in errore con stato feed tracciato.
- Force-poll feed tramite richiesta persistita (`feed_poll_requests`).

### Backlog
- Migliorare ulteriormente l'osservabilita` per feed instabili con diagnostica aggregata in UI.

## job-analyzer

### Attuali
- Consumo da `analyzer_queue` con claim/release/complete persistenti.
- Chiamata LLM via endpoint OpenAI-compatible (`/v1/chat/completions`).
- Estrazione campi job strutturati in JSON.
- Structured output strict JSON Schema (`response_format` + `json_schema`) configurabile via `ANALYZER_LLM_STRICT_JSON`.
- Propagazione link/immagini originali nel payload analizzato.
- Modalita` `thinking` configurabile (`LLM_THINKING_ENABLED`).
- Anti-allucinazione nel prompt: uso solo dati espliciti presenti negli input.
- Ranking qualita` annuncio basato sulla presenza chiara di campi chiave.
- Source-enrichment configurabile (`ANALYZER_SOURCE_EXTRACTOR=off|basic|scrapling|hybrid`) con fallback al solo feed in caso di errore.
- Source-enrichment Scrapling orientato a output markdown (non HTML raw) da passare all'LLM.
- Quarantena item poison dopo soglia tentativi (`ANALYZER_MAX_DELIVERY_ATTEMPTS`).
- Telemetria GenAI semconv su span/metriche (`gen_ai.client.operation.duration`, `gen_ai.client.token.usage`, `gen_ai.request.*`, `gen_ai.response.*`, `gen_ai.usage.*`, `error.type`).

### Backlog
- Fallback opzionale di source-enrichment via browser headless (feature flag) per domini protetti da anti-bot/JS challenge, con rate limit stretto e cache.

## message-dispatcher

### Attuali
- Consumo da `dispatch_queue` con lease/retry semantics.
- Render markdown in stile RWCT Jobs.
- Destinazioni supportate: file sink locale e Telegram.
- Rate limiting configurabile.
- Audit invii e stato item aggiornato in SQLite.
- Requeue supportato via web-admin.

### Backlog
- Estensione a ulteriori destinazioni mantenendo compatibilita` con contract JSON versionato.

## web-admin

### Attuali
- Gestione feed: add/remove/enable/disable.
- Trigger force-poll feed.
- Layout con sidebar sinistra collassabile e registro viste estendibile.
- Vista `Feeds & Items` per gestione feed e azioni item.
- Vista `Pipeline Board` a lane FIFO per stati queue-first (`raw_backlog`, `raw_inflight`, `analyzed_backlog`, `analyzed_inflight`, `failed`, `anomalies`).
- Dashboard pipeline con backlog/inflight per queue raw/analyzed.
- Widget analyzer runtime (modello, endpoint, timeout, token, thinking, limiti operativi).
- Requeue con routing automatico (`mode=analyzed` verso `dispatch_queue`, `mode=raw` verso `analyzer_queue` quando manca payload analizzato).
- Visualizzazione payload processato con JSON formattato.

### Backlog
- Ottimizzazione query dashboard per ridurre pattern N+1.
- Hardening sicurezza endpoint mutativi per uso non-locale.

## llm runtime (llama.cpp / model serving)

### Attuali
- Profilo `prod` con serving modello GGUF locale.
- Parametri modello configurabili via env.
- Supporto toggle `thinking` inoltrato dal servizio analyzer quando abilitato.
- Endpoint Prometheus `/metrics` abilitabile via `LLAMA_ENABLE_METRICS` (`--metrics`).

### Backlog
- Validazione periodica di compatibilita` con ultime release disponibili di `llama.cpp`.

## llm-mock e test utilities

### Attuali
- `llm-mock` per test/dev senza modello reale.
- `test-rss` come mini-sito locale per validazione pipeline: feed RSS e pagine annuncio locali generate a intervallo.
- Suite test unit/integration/e2e su queue semantics, requeue e pipeline.

### Backlog
- Rafforzare i test live-model su scenari ambigui/no-data per verificare non-allucinazione e ranking coerente.

## piattaforma condivisa

### Attuali
- Bus e stato persistente su SQLite condiviso tra servizi.
- Healthcheck e telemetria OpenTelemetry end-to-end (metriche, log, tracce) via `otel-collector`.
- Stack osservabilita` locale completo: `prometheus`, `loki`, `tempo`, `grafana` con datasource/dashboard provisionati.
- Propagazione trace context distribuito nella pipeline asincrona (`traceparent`/`tracestate` nei payload evento).
- Avvio con Docker Compose (`dev`/`prod`).
- Documentazione state machine/lifecycle e troubleshooting operativo.

### Backlog
- Centralizzazione completa schema/migrazioni/config bootstrap in moduli condivisi piu` piccoli.
- Refactor dei servizi monolitici (`store`, `queue`, `service`, `http`, `ui`).
