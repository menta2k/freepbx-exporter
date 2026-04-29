# freepbx-exporter

Prometheus exporter for **FreePBX / Asterisk** that scrapes the
[Asterisk Manager Interface (AMI)](https://docs.asterisk.org/Configuration/Interfaces/Asterisk-Manager-Interface-AMI/).

Written in Go, single binary, no external dependencies on the PBX host.

## Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `asterisk_up` | gauge | – | 1 if the last AMI scrape succeeded |
| `asterisk_scrape_duration_seconds` | gauge | – | Time taken for the AMI scrape |
| `asterisk_scrape_errors_total` | counter | `phase` | Failed scrape phases (dial, login, core_status, channels, sip_peers, pjsip_endpoints, queues) |
| `asterisk_info` | gauge | `version`, `system_name` | Build info (always 1) |
| `asterisk_uptime_seconds` | gauge | – | Seconds since `core start` |
| `asterisk_last_reload_seconds` | gauge | – | Seconds since last reload |
| `asterisk_current_calls` | gauge | – | `CoreStatus.CoreCurrentCalls` |
| `asterisk_channels_active` | gauge | – | Channels currently allocated |
| `asterisk_channels_by_state` | gauge | `state` | Channels grouped by `ChannelStateDesc` |
| `asterisk_sip_peer_up` | gauge | `peer`, `status` | chan_sip peer reachability (1=OK) |
| `asterisk_sip_peer_latency_milliseconds` | gauge | `peer` | chan_sip qualify RTT |
| `asterisk_sip_peers` | gauge | `status` | chan_sip peers grouped by status |
| `asterisk_pjsip_endpoint_up` | gauge | `endpoint`, `device_state`, `kind` | PJSIP endpoint reachability; `kind` is heuristic `trunk`/`extension` |
| `asterisk_pjsip_endpoints` | gauge | `device_state`, `kind` | PJSIP endpoints grouped by device state and kind |
| `asterisk_queue_callers` | gauge | `queue` | Callers waiting in queue |
| `asterisk_queue_completed_calls` | counter | `queue` | Calls completed by queue (since startup) |
| `asterisk_queue_abandoned_calls` | counter | `queue` | Calls abandoned in queue (since startup) |
| `asterisk_queue_members` | gauge | `queue`, `status` | Queue members grouped by status |
| `asterisk_rtcp_jitter_milliseconds` | histogram | `direction`, `channel_type` | Inter-arrival jitter from RTCP events |
| `asterisk_rtcp_rtt_milliseconds` | histogram | `channel_type` | Round-trip time (only when AMI event includes RTT) |
| `asterisk_rtcp_packet_loss_ratio` | gauge | `direction` (and `channel` if `--rtcp-per-channel`) | Last-seen RTCP fraction-lost (0..1) |
| `asterisk_rtcp_events_total` | counter | `direction` | RTCP events observed on the AMI stream |
| `asterisk_rtcp_one_way_audio_total` | counter | `reason` | Heuristic one-way audio detections at hangup |
| `asterisk_event_stream_up` | gauge | – | 1 if persistent AMI event-stream is healthy |
| `asterisk_event_stream_reconnects_total` | counter | – | AMI event-stream reconnect attempts |

## Configuration

Flags or `FREEPBX_EXPORTER_*` environment variables (flags win on conflict).

| Flag | Env | Default | |
|---|---|---|---|
| `-web.listen-address` | `FREEPBX_EXPORTER_LISTEN` | `:9810` | HTTP listen address |
| `-web.telemetry-path` | `FREEPBX_EXPORTER_METRICS_PATH` | `/metrics` | Metrics path |
| `-ami.address` | `FREEPBX_EXPORTER_AMI_ADDRESS` | `127.0.0.1:5038` | AMI host:port |
| `-ami.username` | `FREEPBX_EXPORTER_AMI_USERNAME` | – (required) | AMI user |
| `-ami.secret` | `FREEPBX_EXPORTER_AMI_SECRET` | – (required) | AMI secret |
| `-ami.timeout` | `FREEPBX_EXPORTER_AMI_TIMEOUT` | `10s` | Per-action timeout |
| `-no-sip` | `FREEPBX_EXPORTER_NO_SIP` | `false` | Skip chan_sip scraping |
| `-no-pjsip` | `FREEPBX_EXPORTER_NO_PJSIP` | `false` | Skip PJSIP scraping |
| `-no-queues` | `FREEPBX_EXPORTER_NO_QUEUES` | `false` | Skip queue scraping |
| `-enable-events` | `FREEPBX_EXPORTER_ENABLE_EVENTS` | `true` | Run a persistent AMI event-stream consumer for call-quality metrics |
| `-rtcp-per-channel` | `FREEPBX_EXPORTER_RTCP_PER_CHANNEL` | `false` | Emit per-channel `asterisk_rtcp_packet_loss_ratio` (high cardinality) |
| `-log.level` | `FREEPBX_EXPORTER_LOG_LEVEL` | `info` | debug/info/warn/error |
| `-log.format` | `FREEPBX_EXPORTER_LOG_FORMAT` | `text` | text/json |

Always pass the secret via env file (`/etc/default/freepbx-exporter`) — flags
are visible in `/proc/<pid>/cmdline`.

## AMI permissions

Add a read-only AMI user to `/etc/asterisk/manager.conf`:

```ini
[prometheus]
secret = CHANGE_ME
deny   = 0.0.0.0/0.0.0.0
permit = 127.0.0.1/255.255.255.255
read   = system,call,reporting
write  =
```

Reload AMI:

```sh
asterisk -rx "manager reload"
```

The exporter uses these AMI actions: `Login`, `Logoff`, `CoreSettings`,
`CoreStatus`, `CoreShowChannels`, `SIPpeers`, `PJSIPShowEndpoints`,
`QueueStatus`. Optional modules return `Invalid/unknown command` if not
loaded; that's handled gracefully.

### Call quality (RTCP) — how it works

The exporter runs a **persistent AMI connection** alongside the per-scrape
collector when `-enable-events=true` (the default). It logs in with
`Events: call,reporting` and consumes async `RTCPSent`/`RTCPReceived`/`Hangup`
events. Aggregated metrics are exposed at `/metrics`.

| Metric | Meaning |
|---|---|
| `asterisk_rtcp_jitter_milliseconds` | One observation per RTCP event; field `IAJitter` (or legacy `ReportBlockIAJitter0`). Histogram buckets 1, 2.5, 5, 10, 20, 40, 80, 160, 320 ms |
| `asterisk_rtcp_rtt_milliseconds` | Round-trip time when Asterisk reports it (PJSIP usually does, chan_sip rarely). Buckets 5, 10, 20, 40, 80, 160, 320, 640, 1280 ms |
| `asterisk_rtcp_packet_loss_ratio` | Last-seen `FractionLost` per direction. With `-rtcp-per-channel`, also labeled by `channel` for short-lived debugging |
| `asterisk_rtcp_events_total{direction="sent\|received"}` | Total RTCP events observed; useful as a heartbeat |
| `asterisk_rtcp_one_way_audio_total{reason="no_inbound_rtcp\|no_outbound_rtcp"}` | Heuristic: incremented at `Hangup` if the call lasted ≥ 5 s and one direction never saw a single RTCP event. Sufficient for "calls where one side hears nothing" alerts but not a substitute for full media-flow monitoring |
| `asterisk_event_stream_up` / `asterisk_event_stream_reconnects_total` | Health of the persistent AMI connection (auto-reconnects with 1 s → 30 s exponential backoff) |

Per-channel mode (`-rtcp-per-channel=true`) produces one time-series per
active channel and should only be turned on for short investigation windows
on heavily-loaded PBXs — it can easily exceed 100k series otherwise.

The persistent stream uses the same AMI credentials as the scrape
connection. The required permissions (`read = system,call,reporting`) are
already in `deploy/manager.conf.snippet`. To turn the feature off, set
`FREEPBX_EXPORTER_ENABLE_EVENTS=false` and restart the service.

Notes / limitations:
- RTCP events on Asterisk **11** are sparser than on 16/18+. The parser
  tolerates missing fields and only emits metrics for what's present, so
  `rtt_milliseconds` may stay empty on very old PBXs.
- One-way audio detection is heuristic (RTCP-flow based). For
  ground-truth one-way detection look at media-octet counters in CDR.

### PJSIP `kind` label (trunk vs extension)

PJSIP itself has no native `trunk`/`extension` distinction — both are
`[endpoint]` blocks. AMI doesn't expose a flag either, so the exporter
classifies each endpoint heuristically using the `OutboundAuths`/`Auths`
fields plus a name fallback that matches FreePBX conventions:

1. `OutboundAuths` set → `trunk` (authenticates outbound to a provider).
2. `Auths` set, no outbound → `extension` (accepts inbound auth).
3. Neither side has auth → `trunk` if the endpoint name is non-numeric
   (FreePBX trunks are typically named, e.g. `ITD`, `Kamailio`); otherwise
   `extension`.

Stock FreePBX setups classify cleanly. If you build endpoints by hand in
`pjsip.conf` with unusual auth shapes, expect occasional misclassification.

## Running

```sh
make build
./freepbx-exporter \
    -ami.address 127.0.0.1:5038 \
    -ami.username prometheus \
    -ami.secret "$(cat /etc/freepbx-exporter.secret)"
```

Visit http://localhost:9810/metrics.

### Prometheus scrape config

```yaml
- job_name: freepbx
  static_configs:
    - targets: ['pbx-1.example.com:9810']
  scrape_interval: 30s
  scrape_timeout: 15s
```

### systemd

The shipped Debian (`.deb`) and RPM (`.rpm`) packages install
`/lib/systemd/system/freepbx-exporter.service` and read from
`/etc/default/freepbx-exporter`. Edit that file with your AMI credentials and
`systemctl restart freepbx-exporter`. Both package flavours are produced for
`amd64`/`x86_64` and `arm64`/`aarch64` and attached to each GitHub Release.

## Development

```sh
go test -race ./...
go vet ./...
make build
```

The AMI client and collectors are split into small, focused files under
`internal/`. Tests use an in-process fake AMI server (`internal/ami/client_test.go`)
and a fake `ami.Conn` (`internal/collector/collector_test.go`) so nothing
needs a real Asterisk to run.

## Layout

```
cmd/freepbx-exporter/    main, HTTP server, signal handling
internal/ami/            AMI protocol client (line-based, action/list)
internal/collector/      Prometheus collector + per-subsystem scrapers
internal/config/         Flag + env loading and validation
deploy/                  systemd unit, environment file, AMI snippet
nfpm.yaml                .deb packaging
.github/workflows/       CI: vet, test, build, package, release
```

## License

MIT
