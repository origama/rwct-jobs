# Scrapling Integration (Current State)

## Stato
L'integrazione Scrapling e` implementata in produzione locale del progetto.

- Punto di integrazione analyzer: `internal/analyzer/service.go` (`fetchSourcePageExcerpt(...)`).
- Sidecar Python: `sidecars/scrapling_sidecar/app.py`.
- Strategia runtime: `ANALYZER_SOURCE_EXTRACTOR=off|basic|scrapling|hybrid`.
- Strategia consigliata: `hybrid` (basic first, fallback scrapling su errore o testo corto).

## Comportamento implementato

- `off`: nessun source enrichment.
- `basic`: HTTP GET + estrazione testo da HTML.
- `scrapling`: chiamata sidecar `/extract`.
- `hybrid`:
- prova `basic`.
- se `basic` fallisce o produce testo sotto soglia (`ANALYZER_SOURCE_MIN_CHARS_FOR_BASIC`), prova `scrapling`.
- se `scrapling` fallisce e `basic` aveva prodotto testo non vuoto, mantiene `basic` best effort.

## Contratto HTTP analyzer -> scrapling-sidecar

Endpoint:
- `POST /extract`

Request JSON:

```json
{
  "url": "https://example.com/jobs/123",
  "timeout_ms": 8000,
  "max_chars": 10000,
  "mode": "auto",
  "user_agent": "rwct-agent-analyzer/1.0 (+source-page-scraper)"
}
```

Response JSON:

```json
{
  "ok": true,
  "text": "...markdown estratto...",
  "final_url": "https://example.com/jobs/123",
  "method": "static",
  "status_code": 200,
  "duration_ms": 1840,
  "blocked": false,
  "error": ""
}
```

Note:
- `text` e` markdown normalizzato (non HTML raw), con focus sul contenuto principale quando rilevabile.
- in caso errore il sidecar risponde con `ok=false` e `error` valorizzato.

## Variabili env rilevanti

- `ANALYZER_SOURCE_EXTRACTOR=off|basic|scrapling|hybrid` (default `basic`)
- `ANALYZER_SOURCE_MIN_CHARS_FOR_BASIC=220`
- `ANALYZER_SCRAPLING_ENDPOINT=http://scrapling-sidecar:8088`
- `ANALYZER_SCRAPLING_TIMEOUT=8s`
- `ANALYZER_SCRAPLING_MAX_CHARS=10000`

## Osservabilita`

- Sidecar instrumentato OTel (tracce, metriche, log) verso collector.
- Metriche lato sidecar:
- `rwct_scrapling_extract_requests_total{result}`
- `rwct_scrapling_extract_duration_ms{result}`

## Limiti noti

- Alcuni siti anti-bot/JS challenge possono ancora degradare l'estrazione (`salary` o dettagli secondari mancanti).
- La qualita` del markdown dipende dalla struttura HTML della pagina sorgente.

## Prossimi miglioramenti

- Estrazione selettiva piu` aggressiva di sezioni compensation/benefit su domini noti.
- Eventuale fallback headless dedicato dietro feature flag su domini problematici.
