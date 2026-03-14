# Observability Stack (OTel + Prometheus/Loki/Tempo/Grafana)

## Architettura

Tutti i componenti applicativi inviano segnali OpenTelemetry all'istanza locale `otel-collector`:

- Metriche: OTel SDK -> OTLP gRPC -> `otel-collector` -> exporter Prometheus (`:8889`) -> `prometheus`
- Log: OTel SDK/bridge slog -> OTLP gRPC -> `otel-collector` -> exporter Loki -> `loki`
- Tracce: OTel SDK -> OTLP gRPC -> `otel-collector` -> exporter OTLP -> `tempo`

Grafana e` preconfigurato con datasource Prometheus/Loki/Tempo e dashboard `RWCT Observability Overview`.

## Componenti instrumentati

- `rss-reader`
- `job-analyzer`
- `message-dispatcher`
- `web-admin`
- `test-rss`
- `llm-mock`
- `scrapling-sidecar`

## Propagazione trace distribuite

Il contesto viene propagato lungo pipeline asincrona via payload JSON:

- `traceparent` / `tracestate` in `RawJobItem`
- `traceparent` / `tracestate` in `AnalyzedJob`

Questo consente di seguire il flusso distribuito:
`rss-reader -> job-analyzer -> message-dispatcher`.

## Avvio locale

```bash
docker compose --profile obs up -d
```

Per affiancare observability alla pipeline completa locale:

```bash
docker compose --profile prod --profile obs up -d --build
```

Endpoint utili:

- Grafana: `http://localhost:3000`
- Prometheus: `http://localhost:9090`
- Loki: `http://localhost:3100`
- Tempo: `http://localhost:3200`
- OTel Collector metrics: `http://localhost:8888/metrics`
- OTel Collector exported metrics: `http://localhost:8889/metrics`

## Verifica rapida

Metriche (Prometheus):

- `rwct_rss_items_enqueued_total`
- `rwct_analyzer_jobs_total`
- `rwct_analyzer_job_failures_total`
- `rwct_dispatch_jobs_total`
- `rwct_dispatch_job_failures_total`
- `rwct_scrapling_extract_requests_total`

Log (Loki):

- query esempio:
  `{service_name=~"rss-reader|job-analyzer|message-dispatcher|web-admin|test-rss|scrapling-sidecar|llm-mock"}`

Tracce (Tempo):

- cerca spans per servizio (`service.name`) oppure via trace ID dai log correlati.
- esempio API per trace business:
  `curl -sG 'http://localhost:3200/api/search' --data-urlencode 'tags=name=analyzer.handle_item' --data-urlencode 'limit=5'`

## File di configurazione

- Collector: `observability/collector/config.yaml`
- Prometheus: `observability/prometheus/prometheus.yml`
- Loki: `observability/loki/config.yaml`
- Tempo: `observability/tempo/config.yaml`
- Grafana datasources: `observability/grafana/provisioning/datasources/datasources.yaml`
- Grafana dashboards: `observability/grafana/provisioning/dashboards/dashboards.yaml`
- Dashboard overview: `observability/grafana/dashboards/rwct-overview.json`
