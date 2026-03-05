# RWCT Agent Services (MVP Scaffold)

Microservizi Go per pipeline RSS -> SQLite Queue -> LLM Analyzer -> Dispatcher.

## Servizi

- `rss-reader`: legge feed RSS, deduplica (guid/url/titolo), inserisce item nuovi in `analyzer_queue` (SQLite).
- `job-analyzer`: consuma `analyzer_queue` con lease/ack, usa LLM locale (o mock), produce job strutturati e li mette in `dispatch_queue`.
- `message-dispatcher`: consuma `dispatch_queue` con lease/ack, renderizza template markdown, invia al sink configurato.
- `web-admin`: UI web per gestire feed RSS (add/remove + preview) e dashboard statistiche (categorie, seniority, recenti).

Semantica coda/processed:
- un item nuovo entra in `analyzer_queue` (`state=QUEUED`)
- `job-analyzer` fa claim (`LEASED`), processa e poi `DONE`, inserendo l'output in `dispatch_queue`
- `message-dispatcher` fa claim da `dispatch_queue`, invia e marca item `DISPATCHED`
- su errore la claim viene rilasciata in `QUEUED` con `last_error` (retry controllato)

## Avvio rapido

1. Copia `.env.example` in `.env`
2. Profilo dev:

```bash
cp .env.example .env
docker compose --profile dev up --build
```

Output test messages:

- `/data/outbox/messages.md` nel volume `rwct-data`

Per usare Telegram:

1. imposta in `.env`:
   - `DESTINATION_MODE=telegram`
   - `TELEGRAM_BOT_TOKEN=...`
   - `TELEGRAM_CHAT_ID=...`
2. opzionale: `TELEGRAM_THREAD_ID`, `TELEGRAM_PARSE_MODE`, `TELEGRAM_DISABLE_WEB_PAGE_PREVIEW`

Profilo prod (llama.cpp):

```bash
docker compose --profile prod up --build
```

Nota bootstrap feed-reader:
- `RSS_BOOTSTRAP_MARK_EXISTING=false` (default): su DB vuoto gli item non sono pre-marcati.
- `RSS_COLD_START_ITEMS_PER_FEED` (default `3`): limita quanti item per feed vengono pubblicati in cold start.

## Web Admin

- URL: `http://localhost:8090` (configurabile con `WEB_ADMIN_PORT`)
- Funzioni:
  - aggiunta/rimozione feed RSS persistente su SQLite
  - preview di un feed (titolo + ultimi item)
  - statistiche items ricevuti/analizzati (per categoria e seniority)

## Test

```bash
go test ./...
```
