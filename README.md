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
