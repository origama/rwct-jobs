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
- Propagazione link/immagini originali nel payload analizzato.
- Modalita` `thinking` configurabile (`LLM_THINKING_ENABLED`).
- Anti-allucinazione nel prompt: uso solo dati espliciti presenti negli input.
- Ranking qualita` annuncio basato sulla presenza chiara di campi chiave.
- Scraping opzionale pagina sorgente (`ANALYZER_SCRAPE_SOURCE_PAGE`) con fallback al solo feed in caso di errore.
- Quarantena item poison dopo soglia tentativi (`ANALYZER_MAX_DELIVERY_ATTEMPTS`).

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
- Dashboard pipeline con backlog/inflight per queue raw/analyzed.
- Widget analyzer runtime (modello, endpoint, timeout, token, thinking, limiti operativi).
- Requeue item analizzati.
- Visualizzazione payload processato con JSON formattato.

### Backlog
- Webview multi-scheda con viste operative dedicate per item per feed, item per queue e item per stato.
- Ottimizzazione query dashboard per ridurre pattern N+1.
- Hardening sicurezza endpoint mutativi per uso non-locale.

## llm runtime (llama.cpp / model serving)

### Attuali
- Profilo `prod` con serving modello GGUF locale.
- Parametri modello configurabili via env.
- Supporto toggle `thinking` inoltrato dal servizio analyzer quando abilitato.

### Backlog
- Validazione periodica di compatibilita` con ultime release disponibili di `llama.cpp`.

## llm-mock e test utilities

### Attuali
- `llm-mock` per test/dev senza modello reale.
- `test-rss` per simulazione feed e validazione pipeline.
- Suite test unit/integration/e2e su queue semantics, requeue e pipeline.

### Backlog
- Rafforzare i test live-model su scenari ambigui/no-data per verificare non-allucinazione e ranking coerente.

## piattaforma condivisa

### Attuali
- Bus e stato persistente su SQLite condiviso tra servizi.
- Healthcheck e telemetria OpenTelemetry.
- Avvio con Docker Compose (`dev`/`prod`).
- Documentazione state machine/lifecycle e troubleshooting operativo.

### Backlog
- Centralizzazione completa schema/migrazioni/config bootstrap in moduli condivisi piu` piccoli.
- Refactor dei servizi monolitici (`store`, `queue`, `service`, `http`, `ui`).
