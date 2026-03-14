# Observability Stack (OTel Collector + Grafana Cloud OTLP)

## Architettura

Tutti i componenti applicativi inviano segnali OpenTelemetry all'istanza locale `otel-collector`:

- Metriche: OTel SDK -> OTLP gRPC -> `otel-collector` -> OTLP/HTTP -> Grafana Cloud OTLP Gateway
- Log: OTel SDK/bridge slog -> OTLP gRPC -> `otel-collector` -> OTLP/HTTP -> Grafana Cloud OTLP Gateway
- Tracce: OTel SDK -> OTLP gRPC -> `otel-collector` -> OTLP/HTTP -> Grafana Cloud OTLP Gateway

L'endpoint di destinazione e`:
`https://otlp-gateway-prod-eu-west-0.grafana.net/otlp`

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
docker compose --profile prod up -d --build
```

Il collector parte sempre con il compose (non e` legato al profilo `obs`).

Per avviare anche lo stack locale di visualizzazione:

```bash
docker compose --profile prod --profile obs up -d --build
```

Credenziali richieste in `.env.secrets`:

- `GRAFANA_INSTANCE_ID`
- `GRAFANA_TOKEN`

## Verifica rapida

Collector:

- health: `http://localhost:13133/`
- metrics interne collector: `http://localhost:8888/metrics`
- endpoint Prometheus exporter collector: `http://localhost:8889/metrics`

Tracce/log/metriche:

- verifica su Grafana Cloud (Explore/Traces/Metrics/Logs).

Nota sul profilo `obs`:

- `prometheus`, `loki`, `tempo`, `grafana` locali restano disponibili come stack opzionale.
- il forwarding principale del collector resta verso Grafana Cloud OTLP.

## File di configurazione

- Collector: `observability/collector/config.yaml`
- Prometheus: `observability/prometheus/prometheus.yml`
- Loki: `observability/loki/config.yaml`
- Tempo: `observability/tempo/config.yaml`
- Grafana datasources: `observability/grafana/provisioning/datasources/datasources.yaml`
- Grafana dashboards: `observability/grafana/provisioning/dashboards/dashboards.yaml`
- Dashboard overview: `observability/grafana/dashboards/rwct-overview.json`
