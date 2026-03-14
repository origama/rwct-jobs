# Scrapling Integration Plan (Experiment)

## Obiettivo
Ridurre i fallimenti di source-enrichment nel `job-analyzer` quando la pagina sorgente e` protetta da JS challenge/anti-bot o richiede rendering dinamico.

## Punto di integrazione
Il punto unico da estendere e` la funzione di estrazione testo pagina sorgente:
- `internal/analyzer/service.go`: `fetchSourcePageExcerpt(...)`

L'orchestrazione `analyze(...)` resta invariata (stesso fallback finale al solo feed in caso errore).

## Strategia proposta
Introdurre una strategia configurabile di estrazione:
- `off`: disabilita source-enrichment (solo dati feed)
- `basic`: comportamento attuale (HTTP GET + strip HTML)
- `scrapling`: usa sidecar Scrapling via HTTP
- `hybrid` (consigliata): prova `basic`, fallback a `scrapling` su errore o estratto troppo povero

## Perche' sidecar (e non embedding diretto)
`Scrapling` e` una libreria Python con dipendenze/runtime dedicati. Tenerla in sidecar:
- evita di complicare il container Go dell'analyzer
- rende piu' semplice tuning indipendente (timeout, retries, browser deps)
- permette rollout graduale senza cambiare il path principale del servizio

## Contratto HTTP analyzer -> scrapling-sidecar
Endpoint:
- `POST /extract`

Request JSON:
```json
{
  "url": "https://example.com/jobs/123",
  "timeout_ms": 7000,
  "max_chars": 2500,
  "mode": "auto",
  "user_agent": "rwct-agent-analyzer/1.0 (+source-page-scraper)"
}
```

Note:
- `mode` inizialmente `auto` (la logica nel sidecar decide static/dynamic/stealthy)
- `max_chars` deve allinearsi al clamp attuale in analyzer (2500)

Response JSON (success):
```json
{
  "ok": true,
  "text": "...estratto normalizzato...",
  "final_url": "https://example.com/jobs/123",
  "method": "dynamic",
  "status_code": 200,
  "duration_ms": 1840,
  "blocked": false,
  "error": ""
}
```

Response JSON (errore):
```json
{
  "ok": false,
  "text": "",
  "final_url": "https://example.com/jobs/123",
  "method": "stealthy",
  "status_code": 403,
  "duration_ms": 2200,
  "blocked": true,
  "error": "challenge/blocked"
}
```

## Nuove env var (analyzer)
- `ANALYZER_SOURCE_EXTRACTOR=off|basic|scrapling|hybrid` (default `basic`)
- `ANALYZER_SOURCE_MIN_CHARS_FOR_BASIC=220` (solo per `hybrid`)
- `ANALYZER_SCRAPLING_ENDPOINT=http://scrapling-sidecar:8088`
- `ANALYZER_SCRAPLING_TIMEOUT=8s`
- `ANALYZER_SCRAPLING_MAX_CHARS=2500`

Comportamento:
- `off`: nessuna chiamata di source extraction 
- altri valori: usa la strategia configurata da `ANALYZER_SOURCE_EXTRACTOR`


## Regole di fallback (hybrid)
1. Tenta `basic`
2. Se `basic` fallisce, prova `scrapling`
3. Se `basic` ha successo ma `len(text) < ANALYZER_SOURCE_MIN_CHARS_FOR_BASIC`, prova `scrapling`
4. Se anche `scrapling` fallisce, log warn e continua con solo feed

## Log e osservabilita' minime
Aggiungere campi strutturati nel warn/info di scraping:
- `extractor_strategy`
- `extractor_method` (`basic`, `static`, `dynamic`, `stealthy`)
- `extractor_latency_ms`
- `extractor_chars`
- `extractor_fallback_used`
- `source_domain`

## Rollout esperimento in branch
Fase 1 (shadow mode):
- analyzer usa ancora `basic` nel prompt
- chiama scrapling in parallelo solo per metriche/comparazione (senza impatto output)

Fase 2 (active hybrid):
- `ANALYZER_SOURCE_EXTRACTOR=hybrid`
- l'output scrapling entra nel prompt solo nei casi di fallback

## Criteri di successo (go/no-go)
Confronto su stesso set feed/domìni problematici:
- aumento pagine con estratto utile (`chars >= 220`): target `+20%`
- riduzione warning scraping failed: target `-30%`
- incremento latenza media analyze per item: massimo `+20%`
- nessun aumento significativo di item `FAILED`

## Test minimi da aggiungere
- Unit: selector strategia (`off/basic/scrapling/hybrid`)
- Unit: fallback `hybrid` su errore e su testo corto
- Unit: parser response sidecar + gestione errori timeout/status
- Integration: fake sidecar HTTP nel test analyzer

## Impatto su docker-compose (quando implementiamo)
Aggiungere servizio:
- `scrapling-sidecar` su porta interna `8088`
- dipendenze Python/Playwright secondo runtime scelto

`job-analyzer`:
- dipendenza health su sidecar solo se `ANALYZER_SOURCE_EXTRACTOR` include scrapling

## Decisioni aperte da confermare prima del coding
1. Threshold testo corto: `220` caratteri va bene come default?
2. In Fase 1 shadow mode vogliamo campionamento (`10%-30%`) o sempre-on?
3. In caso timeout sidecar, preferiamo retry singolo o fail-fast?
