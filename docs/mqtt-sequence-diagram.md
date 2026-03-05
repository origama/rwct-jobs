# SQLite Queue Sequence Diagram

Questo diagramma descrive la comunicazione tra i servizi via code SQLite condivise.

```mermaid
sequenceDiagram
    autonumber
    participant R as rss-reader
    participant DB as sqlite (shared db)
    participant A as job-analyzer
    participant D as message-dispatcher
    participant W as web-admin

    Note over R,A: Queue raw: analyzer_queue
    Note over A,D: Queue analyzed: dispatch_queue

    loop Polling periodico feed
        R->>R: Estrae nuovi item RSS + dedup (guid/url/title)
        alt Nuovo item
            R->>DB: INSERT analyzer_queue(state=QUEUED)
            A->>DB: CLAIM analyzer_queue -> LEASED (1 item)
            A->>A: Analisi annuncio + categorizzazione
            A->>DB: UPDATE rss_items(status=ANALYZED,payload)
            A->>DB: INSERT dispatch_queue(state=QUEUED)
            A->>DB: COMPLETE analyzer_queue -> DONE
            D->>DB: CLAIM dispatch_queue -> LEASED (1 item)
            D->>D: Rendering template + invio destinazione (Telegram/file)
            D->>DB: UPDATE rss_items(status=DISPATCHED)
            D->>DB: COMPLETE dispatch_queue -> DONE
            W->>DB: Query monitor/overview/feeds/items
        else Duplicato
            R->>R: Skip publish + log duplicate item skipped
        end
    end

    alt Errore analyzer
        A->>DB: RELEASE analyzer_queue -> QUEUED + last_error
    else Errore dispatcher
        D->>DB: RELEASE dispatch_queue -> QUEUED + last_error
    end
```

## Queue Tabelle

- `analyzer_queue`: coda item raw (`QUEUED`,`LEASED`,`DONE`)
- `dispatch_queue`: coda item analizzati (`QUEUED`,`LEASED`,`DONE`)
- `rss_items`: stato business (`NEW`,`ANALYZED`,`DISPATCHED`,`FAILED`) e payload/versioni
