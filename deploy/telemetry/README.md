# Osmia telemetry stack

A self-hosted Docker Compose stack that receives Osmia's OpenTelemetry traces
and lets you query them:

- **Collector** (`otel/opentelemetry-collector-contrib`) receives OTLP on gRPC
  (`4317`) and HTTP (`4318`) and exports traces to ClickHouse. The exporter's
  sending queue is stored on disk by the `file_storage` extension, on the
  `collector-queue` volume, so spans the collector accepted but could not yet
  write survive a collector restart or a ClickHouse outage.
- **ClickHouse** stores the spans (volume `clickhouse-data`).
- **Grafana** (port `3000`) has the `grafana-clickhouse-datasource` plugin
  installed at start and a provisioned data source named `Osmia ClickHouse`
  pointing at the Compose ClickHouse. There is nothing to configure by hand.
  No dashboards are provisioned.

## Start

```sh
docker compose -f deploy/telemetry/compose.yaml up -d
```

The first start downloads the images and the Grafana plugin, so Grafana may
take a minute before it answers. Open <http://127.0.0.1:3000> and sign in with
the Grafana credentials below, then use Explore with the `Osmia ClickHouse`
data source.

Stop it with `docker compose -f deploy/telemetry/compose.yaml down`; add `-v`
to also delete the stored data and the collector queue.

## Point Osmia at it

In Osmia's configuration, enable telemetry with the collector's OTLP endpoint
(see [docs/configuration.md](../../docs/configuration.md) for the `[telemetry]`
table):

```toml
[telemetry]
endpoint = "127.0.0.1:4317"
protocol = "grpc"
insecure = true
```

For OTLP/HTTP use `127.0.0.1:4318` with `protocol = "http"`. The local stack
does not use TLS or authentication on the collector, hence `insecure = true`;
it listens on loopback only unless you change `TELEMETRY_BIND`.

## Configuration

All settings are environment variables read by Compose, with defaults meant
for a local, loopback-only stack. Nothing secret is committed; set these in
your shell or an untracked `.env` file next to `compose.yaml` to override them.

| Variable | Default | Purpose |
| --- | --- | --- |
| `CLICKHOUSE_USER` | `osmia` | ClickHouse user used by the collector and Grafana |
| `CLICKHOUSE_PASSWORD` | `osmia-local` | Password for that user |
| `CLICKHOUSE_DATABASE` | `otel` | Database holding the trace tables |
| `GRAFANA_ADMIN_USER` | `admin` | Grafana admin login |
| `GRAFANA_ADMIN_PASSWORD` | `admin` | Grafana admin password |
| `TELEMETRY_BIND` | `127.0.0.1` | Host address the published ports bind to |

Change the passwords before binding to anything other than loopback.

## Tables

The collector's ClickHouse exporter creates its schema on first start in the
`CLICKHOUSE_DATABASE` database:

- `otel_traces`: one row per span. Columns include `Timestamp` (span start),
  `TraceId`, `SpanId`, `ParentSpanId`, `SpanName`, `SpanKind`, `ServiceName`,
  `ResourceAttributes`, `SpanAttributes` (a `Map(String, String)`, so every
  attribute value reads as a string), `Duration` (nanoseconds), `StatusCode`,
  `StatusMessage`, and the `Events.*` and `Links.*` arrays.
- `otel_traces_trace_id_ts`: per-trace start and end times, filled by the
  materialized view `otel_traces_trace_id_ts_mv`.

Queries about Osmia spans read `otel_traces`; [docs/telemetry.md](../../docs/telemetry.md)
lists the spans and gives four worked queries. The collector listens on
`4317`/`4318` and ClickHouse's HTTP interface on `8123` for ad-hoc queries:

```sh
curl -s --user osmia:osmia-local \
  'http://127.0.0.1:8123/?database=otel' \
  --data 'SELECT SpanName, count() FROM otel_traces GROUP BY SpanName'
```
