# Lifecycle e State Machine

Questo documento descrive i lifecycle implementati nel codice corrente.
La pipeline usa solo code SQLite (`analyzer_queue`, `dispatch_queue`) su DB condiviso.

## Pipeline end-to-end

```mermaid
sequenceDiagram
    autonumber
    participant R as rss-reader
    participant DB as sqlite
    participant A as job-analyzer
    participant D as message-dispatcher
    participant W as web-admin

    R->>DB: INSERT rss_items(status=NEW)
    R->>DB: UPSERT analyzer_queue(state=QUEUED)
    A->>DB: CLAIM analyzer_queue -> LEASED
    A->>A: LLM analyze (feed + source page scraping opzionale)
    A->>DB: UPDATE rss_items(status=ANALYZED,payload)
    A->>DB: UPSERT dispatch_queue(state=QUEUED)
    A->>DB: COMPLETE analyzer_queue -> DONE
    D->>DB: CLAIM dispatch_queue -> LEASED
    D->>D: Render template + deliver (file/telegram)
    D->>DB: UPDATE rss_items(status=DISPATCHED)
    D->>DB: COMPLETE dispatch_queue -> DONE
    W->>DB: query monitor/feeds/items/runtime
```

## `analyzer_queue` state machine

```mermaid
stateDiagram-v2
    [*] --> QUEUED: enqueue raw item
    QUEUED --> LEASED: claimNextRawJob
    LEASED --> DONE: completeClaim (analyzed+dispatch enqueued)
    LEASED --> QUEUED: releaseClaim (errore retryable)
    LEASED --> DONE: failClaim (poison item/non-retryable/max attempts)
```

Note implementative:
- claim con lease timeout (`lease_until`) e retry lease-expired.
- anti-stallo: `ANALYZER_MAX_DELIVERY_ATTEMPTS` + quarantena errori non-retryable (`missing required analyzed fields`).

## `dispatch_queue` state machine

```mermaid
stateDiagram-v2
    [*] --> QUEUED: enqueue analyzed payload
    QUEUED --> LEASED: claimNextDispatchJob
    LEASED --> DONE: completeDispatchClaim (deliver ok)
    LEASED --> QUEUED: releaseDispatchClaim (deliver failed)
```

## `rss_items.status` lifecycle

```mermaid
stateDiagram-v2
    [*] --> NEW: insertItemIfNew
    NEW --> ANALYZED: analyzer markItemStatus
    NEW --> FAILED: analyzer markItemStatus (analysis error)
    ANALYZED --> DISPATCHED: dispatcher markItemStatus
    ANALYZED --> FAILED: dispatcher markItemStatus (deliver error)
    DISPATCHED --> ANALYZED: web-admin requeue analyzed payload
```

Note:
- `analyzed_payload_json` e `analyzed_at` vengono valorizzati in `ANALYZED`.
- `dispatched_at` viene valorizzato in `DISPATCHED`.

## Feed polling lifecycle (`feed_state`)

```mermaid
stateDiagram-v2
    [*] --> POLLING_ENABLED: feed assente in feed_state o down_until scaduto
    POLLING_ENABLED --> COOLDOWN: markDown su errore poll
    COOLDOWN --> POLLING_ENABLED: markUp su poll riuscito o timeout scaduto
```

Dettagli:
- `markDown` imposta `failure_count=5` e `down_until=now+RSS_COOLDOWN_RETRY`.
- `markUp` resetta `failure_count=0` e `down_until=NULL`.

## Requeue lifecycle (web-admin)

```mermaid
sequenceDiagram
    autonumber
    participant UI as web-admin UI
    participant API as web-admin API
    participant DB as sqlite
    participant D as message-dispatcher

    UI->>API: POST /api/db/items/requeue {id}
    API->>DB: read analyzed_payload_json
    API->>DB: UPSERT dispatch_queue(state=QUEUED,payload=analyzed)
    API->>DB: UPDATE rss_items(status=ANALYZED, dispatched_at=NULL)
    D->>DB: CLAIM dispatch_queue -> LEASED
    D->>DB: COMPLETE -> DONE + rss_items=DISPATCHED
```

