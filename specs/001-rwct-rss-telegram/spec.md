# Feature Specification: RWCT Job Feeds Agent (MVP)

**Feature Branch**: `001-rwct-rss-telegram`  
**Created**: 2026-03-04  
**Status**: Draft  
**Input**: User description: "Sistema a microservizi (Go) con RSS reader, job analyzer LLM locale e message dispatcher, avviabile con Docker Compose"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Ingestion + Dedup + Dispatch (Priority: P1)

Come maintainer RWCT voglio che il sistema legga i feed RSS ogni 15 minuti, riconosca gli item nuovi e invii i job processati a Telegram (o file sink test), senza duplicazioni e includendo nel messaggio finale i link e le immagini estratte dall'item originale.

**Why this priority**: E' il flusso core MVP end-to-end che genera valore immediato alla community.

**Independent Test**: Avvio `docker compose`, simulo update RSS con nuovo item, verifico che venga accodato/analizzato/inviato una sola volta nel sink test e non venga reinviato ai run successivi.

**Acceptance Scenarios**:

1. **Given** lista feed valida, **When** arriva un nuovo item RSS, **Then** l'item viene inserito in `analyzer_queue`, analizzato e consegnato alla destinazione configurata tramite `dispatch_queue`.
2. **Given** item già processato (stesso guid/url/titolo), **When** il feed viene riletto, **Then** il sistema non ripubblica e registra un log di duplicate skip.
3. **Given** parser analyzer fallisce, **When** l'item non è parseabile, **Then** il messaggio viene scartato e viene emesso log strutturato di errore.
4. **Given** item RSS con hyperlink e immagini, **When** il messaggio finale viene renderizzato, **Then** deve includere almeno il link principale e la lista di link/immagini originali disponibili.

---

### User Story 2 - Affidabilità operativa feed (Priority: P2)

Come maintainer RWCT voglio che i feed in errore vengano ritentati con backoff e marcati esplicitamente come down dopo 5 tentativi, per evitare loop infiniti e avere stato chiaro.

**Why this priority**: Riduce rumore operativo e previene consumo inutile di risorse su feed non affidabili.

**Independent Test**: Simulo un feed non raggiungibile e verifico 5 retry esponenziali, stato feed=down persistito, stop polling per quel feed e log evento.

**Acceptance Scenarios**:

1. **Given** un feed non raggiungibile, **When** il polling fallisce ripetutamente, **Then** il sistema applica retry con backoff esponenziale fino a 5 tentativi.
2. **Given** quinto tentativo fallito, **When** il feed viene marcato down, **Then** il polling per quel feed viene interrotto e l'evento è loggato con causa.

---

### User Story 3 - Observability + Configurabilità run (Priority: P3)

Come maintainer RWCT voglio controllare frequenza polling, limiti uso LLM, rate limit dispatcher e retention tramite env vars, con metriche/log OpenTelemetry.

**Why this priority**: Consente gestione costi/risorse CPU-only e troubleshooting senza cambiare codice.

**Independent Test**: Imposto env vars custom e verifico comportamento (polling interval, rate limit, retention cleanup, endpoint OTel).

**Acceptance Scenarios**:

1. **Given** env vars di configurazione, **When** i servizi si avviano, **Then** applicano i valori senza rebuild.
2. **Given** endpoint OTel configurato, **When** il sistema processa job e errori, **Then** emette log e metriche con attributi coerenti.

### Edge Cases

- Feed con XML valido ma senza `guid`: usare fallback su URL canonica e poi titolo normalizzato.
- Feed con item duplicati nello stesso polling batch.
- Riavvio servizio durante processamento in-flight: evitare doppio invio grazie a stato persistito/idempotenza.
- Telegram non raggiungibile: routing su retry/DLQ senza perdere payload.
- LLM locale in timeout o saturazione CPU: scarto controllato + metriche + log.
- Item con HTML malformato nel campo description/content: estrarre comunque il massimo numero possibile di link/immagini validi senza bloccare la pipeline.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Il sistema MUST eseguire come 3 microservizi Go separati: `rss-reader`, `job-analyzer`, `message-dispatcher`.
- **FR-002**: Il sistema MUST usare SQLite condiviso come bus di integrazione tra servizi, con code applicative persistenti.
- **FR-003**: Il sistema MUST supportare polling RSS di default ogni 15 minuti, override via env var.
- **FR-004**: Il servizio `rss-reader` MUST leggere una lista feed da configurazione esterna (file o env) senza rebuild.
- **FR-005**: Il servizio `rss-reader` MUST deduplicare su 3 livelli: `guid`, URL, titolo normalizzato.
- **FR-006**: In caso di item già processato, il sistema MUST non ripubblicare e MUST emettere log strutturato di skip.
- **FR-007**: Per feed non raggiungibile, `rss-reader` MUST applicare 5 retry con backoff esponenziale; al 5° fallimento MUST marcare feed `down`, loggare e fermare polling per quel feed.
- **FR-008**: `job-analyzer` MUST usare un modello LLM self-hosted locale via container (llama.cpp server o equivalente), ottimizzato per CPU-only.
- **FR-009**: `job-analyzer` MUST estrarre almeno: ruolo, seniority, location, remoto, tech stack, contratto, salary (se presente), lingua, company.
- **FR-010**: Se il parsing LLM fallisce, `job-analyzer` MUST rilasciare il job in coda (`state=QUEUED`) con `last_error` e loggare il motivo.
- **FR-011**: `message-dispatcher` MUST supportare almeno due destinazioni iniziali: Telegram e file sink locale di test.
- **FR-012**: `message-dispatcher` MUST supportare template Markdown configurabile per formattazione output.
- **FR-013**: `message-dispatcher` MUST applicare rate limiting configurabile via env var.
- **FR-014**: Il payload dei job MUST essere JSON versionato (`schema_version`) persistito in SQLite.
- **FR-015**: Il sistema MUST prevedere due code separate con delivery singolo consumer: `analyzer_queue` e `dispatch_queue`.
- **FR-016**: Il sistema MUST persistere stato minimo e audit su SQLite (item processati, feed status, delivery log, errori).
- **FR-017**: Il sistema MUST eseguire cleanup annunci/log in base a retention ore configurabile via env var.
- **FR-018**: Il progetto MUST essere avviabile con un unico `docker-compose.yml` con profili `dev` e `prod`.
- **FR-019**: Tutti i servizi MUST esporre healthcheck significativi per readiness/liveness in Compose.
- **FR-020**: Tutti i servizi MUST emettere log e metriche via OpenTelemetry SDK con endpoint configurabile via env var.
- **FR-021**: I segreti MUST essere caricati da file `.env` (MVP).
- **FR-022**: Il sistema MUST includere test: unit test su queue semantics (claim/release/complete), test requeue, test E2E con feed simulato + verifica sink file.
- **FR-023**: Il sistema SHOULD essere predisposto a deduplicazione semantica futura.
- **FR-024**: Il sistema SHOULD essere predisposto a future destinazioni oltre Telegram/file.
- **FR-025**: Il sistema MAY supportare broker/event-bus esterni in futuro, mantenendo SQLite come source of truth.
- **FR-026**: `rss-reader` MUST estrarre dagli item RSS i link (`href`) e le immagini (`src`/enclosure image) quando presenti e includerli nell'evento raw.
- **FR-027**: `job-analyzer` MUST propagare link e immagini originali nel payload analizzato verso il dispatcher.
- **FR-028**: Il template Markdown di `message-dispatcher` MUST seguire un layout stile RWCT-JOBS (header brand + source label + link principale + corpo annuncio) e MUST includere se disponibili una sezione link originali e una sezione immagini.
- **FR-029**: Il web-admin MUST permettere il requeue di un item analizzato reinserendolo in `dispatch_queue`; l'operazione MUST azzerare `dispatched_at`.
- **FR-030**: Il web-admin MUST permettere il force-poll di un feed tramite richiesta persistita (`feed_poll_requests`) e aggiornare contestualmente la view.
- **FR-031**: Il progetto MUST centralizzare schema e migrazioni SQLite in un modulo condiviso, eliminando la duplicazione della definizione DB tra servizi.
- **FR-032**: Il web-admin MUST prevedere un meccanismo minimo di protezione accessi per le operazioni mutative (auth, binding locale o reverse proxy documentato) prima dell'uso fuori ambiente locale.
- **FR-033**: Il codice SHOULD separare responsabilita` applicative e infrastrutturali, riducendo i file monolitici e spostando UI/assets del web-admin fuori dal sorgente inline.
- **FR-034**: Il bootstrap/config dei servizi SHOULD essere consolidato in un package condiviso con validazione esplicita delle env vars, evitando `panic` su configurazioni invalide.
- **FR-035**: Tutti i servizi SHOULD gestire shutdown e telemetria in modo coerente, con health server chiudibile e policy distinte per ambienti dev/prod.
- **FR-036**: `job-analyzer` MUST esporre in configurazione la modalita` `thinking` del modello e inoltrarla alla richiesta LLM usando il toggle supportato dal backend/modello solo quando esplicitamente abilitata.
- **FR-037**: Il web-admin SHOULD evitare query N+1 sulle dashboard e usare query aggregate per feed/items/metriche.
- **FR-038**: Le capability dichiarate in configurazione MUST riflettere il comportamento reale del runtime; parametri non supportati operativamente non devono essere esposti o devono essere implementati.

### Candidate RSS Seed List (validated 2026-03-04)

- https://weworkremotely.com/remote-jobs.rss
- https://weworkremotely.com/categories/remote-full-stack-programming-jobs.rss
- https://remotive.com/remote-jobs/feed
- https://remotive.com/remote-jobs/feed/software-development
- https://jobicy.com/feed/job_feed
- https://jobspresso.co/feed/

### Key Entities *(include if feature involves data)*

- **FeedSource**: feed configurato da monitorare (`id`, `url`, `status`, `poll_interval`, `failure_count`, `next_poll_at`, `disabled_reason`).
- **RawJobItem**: item RSS grezzo (`feed_id`, `source_label`, `guid`, `url`, `title`, `description`, `links[]`, `image_urls[]`, `published_at`, `fetched_at`).
- **AnalyzedJob**: risultato LLM (`schema_version`, `source_label`, `role`, `company`, `seniority`, `location`, `remote_type`, `tech_stack`, `contract_type`, `salary`, `language`, `summary_it`, `original_links[]`, `original_images[]`, `confidence`).
- **AnalyzerQueueItem**: item raw accodato (`item_id`, `payload_json`, `state`, `lease_owner`, `lease_until`, `delivery_count`, `last_error`).
- **DispatchQueueItem**: item analizzato accodato (`item_id`, `payload_json`, `state`, `lease_owner`, `lease_until`, `delivery_count`, `last_error`).
- **DispatchMessage**: payload pronto per delivery (`destination`, `template_id`, `rendered_markdown`, `rate_limit_bucket`, `attempt`).
- **DeliveryRecord**: audit invio (`message_id`, `destination`, `target`, `status`, `provider_response`, `sent_at`).
- **FeedPollRequest**: richiesta force-poll feed (`feed_url`, `requested_at`).
- **ProcessingError**: errore di processing (`stage`, `queue`, `item_id`, `error_code`, `error_message`, `retryable`, `created_at`).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Con 5-100 annunci/giorno, almeno il 99% degli item nuovi viene processato end-to-end entro 5 minuti dalla pubblicazione su feed (in ambiente MVP).
- **SC-002**: Tasso di duplicati inviati verso destinazioni <= 0.1% su finestra settimanale.
- **SC-003**: Almeno il 95% dei job processati include correttamente i campi minimi obbligatori (`role`, `company`, `location/remote`, `summary_it`).
- **SC-004**: In caso di feed down, lo stato viene marcato e visibile nei log entro massimo 5 tentativi falliti consecutivi.
- **SC-005**: `docker compose --profile dev up` porta tutti i servizi in stato healthy senza interventi manuali oltre `.env`.
- **SC-006**: Suite test minima (unit + queue claim/release/complete + requeue + E2E) eseguibile in CI locale con esito verde.
- **SC-007**: Per item che contengono hyperlink/immagini nel feed, almeno il 95% dei messaggi finali include correttamente i link e le immagini estratte.
- **SC-008**: Tutti i servizi avviano lo stesso schema SQLite attraverso un entrypoint unico di migrazione, senza drift tra ambienti o processi.
- **SC-009**: Il web-admin esposto fuori localhost rifiuta richieste non autorizzate alle operazioni di modifica/cancellazione.
- **SC-010**: La modalita` `thinking` del modello e` configurabile via env senza rebuild e il flag viene incluso nelle richieste LLM solo quando abilitato.

## Post-MVP Hardening Backlog

- Centralizzare schema, DSN SQLite e migrazioni in un package condiviso (`internal/db` o equivalente).
- Introdurre protezione per `web-admin` su endpoint mutativi e documentare il deployment sicuro.
- Rifattorizzare i servizi monolitici in componenti piu` piccoli (`store`, `queue`, `service`, `http`, `ui`).
- Estrarre HTML/CSS/JS del `web-admin` in asset embedded o file dedicati.
- Consolidare parsing/validazione env in un package comune.
- Sostituire i `MustEnv*` con validazione esplicita e errori di startup leggibili.
- Uniformare startup/shutdown di health server e telemetry.
- Allineare configurazioni dichiarate e capability reali del runtime, inclusa la concorrenza analyzer.
- Ottimizzare le query del `web-admin` per evitare pattern N+1 sulle dashboard.
- Aggiungere fallback opzionale di source-enrichment via browser headless (feature flag), da usare solo su domini bloccati da anti-bot/JS challenge con rate limit stretto, cache e fallback sicuro al solo contenuto RSS senza inferenze.
- Introdurre nel `web-admin` una webview multi-scheda per esplorazione operativa: viste dedicate per item per feed, item per queue e item per stato con filtri coerenti e aggiornamento affidabile.
