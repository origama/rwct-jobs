# Troubleshooting

## Queue bloccata (RAW)

Sintomi:
- `consumer_blocked_raw=true` in `/api/monitor/queues`
- `raw_backlog > 0` e/o `raw_inflight > 0` per molti minuti
- stessi `item_id` che falliscono ripetutamente nei log analyzer

### 1) Diagnosi rapida

```bash
curl -sS http://localhost:8090/api/monitor/queues | jq '.queues,.pipeline'
docker logs --since 10m rwct-agent-job-analyzer-3
```

### 2) Sblocco immediato (quarantena item stantii)

Questa procedura marca come `FAILED` e chiude i claim RAW bloccati da oltre 30 minuti.

```bash
docker run --rm -v rwct-agent_rwct-data:/data alpine sh -lc \
"apk add --no-cache sqlite >/dev/null && sqlite3 /data/rss-reader.db \
\"UPDATE rss_items
   SET status='FAILED',
       last_error='manual_unblock: quarantined stale raw queue item',
       updated_at=datetime('now')
 WHERE id IN (
   SELECT item_id
   FROM analyzer_queue
   WHERE state IN ('QUEUED','LEASED')
     AND enqueued_at <= datetime('now','-30 minutes')
 );

 UPDATE analyzer_queue
    SET state='DONE',
        lease_owner=NULL,
        lease_until=NULL,
        done_at=datetime('now'),
        last_error='manual_unblock: quarantined stale raw queue item',
        updated_at=datetime('now')
  WHERE state IN ('QUEUED','LEASED')
    AND enqueued_at <= datetime('now','-30 minutes');\""
```

Verifica:

```bash
curl -sS http://localhost:8090/api/monitor/queues | jq '.queues,.pipeline'
```

Atteso:
- `raw_backlog=0`
- `raw_inflight=0`
- `consumer_blocked_raw=false`

### 3) Prevenzione

Impostare `ANALYZER_MAX_DELIVERY_ATTEMPTS` (default `3`) per evitare loop infinito su poison item.

## SQLite corrotto (`database disk image is malformed`)

### 1) Backup file DB

```bash
docker run --rm -v rwct-agent_rwct-data:/data alpine sh -lc \
"cp /data/rss-reader.db /data/rss-reader.db.bak.$(date +%Y%m%d-%H%M%S)"
```

### 2) Integrita'

```bash
docker run --rm -v rwct-agent_rwct-data:/data alpine sh -lc \
"apk add --no-cache sqlite >/dev/null && sqlite3 /data/rss-reader.db 'PRAGMA integrity_check;'"
```

Se output diverso da `ok`, procedere con recover.

### 3) Recover con `.recover`

```bash
docker run --rm -v rwct-agent_rwct-data:/data alpine sh -lc \
"apk add --no-cache sqlite >/dev/null && \
 sqlite3 /data/rss-reader.db '.recover' | sqlite3 /data/rss-reader.recovered.db"
```

### 4) Sostituzione atomica

```bash
docker compose down
docker run --rm -v rwct-agent_rwct-data:/data alpine sh -lc \
"mv /data/rss-reader.db /data/rss-reader.db.corrupted.$(date +%Y%m%d-%H%M%S) && \
 mv /data/rss-reader.recovered.db /data/rss-reader.db && \
 rm -f /data/rss-reader.db-wal /data/rss-reader.db-shm"
docker compose up -d
```

## Docker storage corrotto (`input/output error` su containerd/blob)

Sintomi:
- `input/output error` su blob path containerd
- `docker run`/`docker exec` falliscono anche su container base

### 1) Recovery leggera

1. Chiudere Docker Desktop completamente.
2. Riavviare Docker Desktop.
3. Verificare:

```bash
docker ps
docker run --rm hello-world
```

### 2) Pulizia immagini/cache (se ancora instabile)

```bash
docker system prune -af
docker builder prune -af
```

### 3) Ultima risorsa

- Reset di Docker Desktop data (cancella immagini/container/volumi locali).
- Ripristinare il DB da backup (`rss-reader.db.bak.*`) nel volume `rwct-agent_rwct-data`.

