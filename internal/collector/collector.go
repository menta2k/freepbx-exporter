// Package collector implements a Prometheus collector that scrapes a single
// Asterisk/FreePBX AMI instance per /metrics request.
package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/menta2k/freepbx-exporter/internal/ami"
)

// Namespace prefix for all metrics.
const namespace = "asterisk"

// ScrapeOptions toggles which AMI actions are issued per scrape. Defaults
// (zero value) enable everything.
type ScrapeOptions struct {
	DisableSIP    bool
	DisablePJSIP  bool
	DisableQueues bool
}

// Collector implements prometheus.Collector. It is safe for concurrent use:
// each Collect call opens its own AMI connection.
type Collector struct {
	dialer  ami.Dialer
	cfg     ami.Config
	options ScrapeOptions
	logger  *slog.Logger

	// Metric descriptors.
	up             *prometheus.Desc
	scrapeDuration *prometheus.Desc
	info           *prometheus.Desc
	uptime         *prometheus.Desc
	lastReload     *prometheus.Desc
	currentCalls   *prometheus.Desc

	channels        *prometheus.Desc
	channelsByState *prometheus.Desc
	callsActive     *prometheus.Desc

	sipPeerUp        *prometheus.Desc
	sipPeerLatencyMs *prometheus.Desc
	sipPeerCount     *prometheus.Desc

	pjsipEndpointUp    *prometheus.Desc
	pjsipEndpointCount *prometheus.Desc

	queueCallers   *prometheus.Desc
	queueCompleted *prometheus.Desc
	queueAbandoned *prometheus.Desc
	queueMembers   *prometheus.Desc

	// Lifetime counters survive across scrapes.
	scrapeErrors *prometheus.CounterVec

	mu sync.Mutex // serializes Collect for stable counter behavior
}

// New constructs a Collector. dialer may be nil to use the default TCP dialer.
func New(cfg ami.Config, opts ScrapeOptions, logger *slog.Logger, dialer ami.Dialer) *Collector {
	if logger == nil {
		logger = slog.Default()
	}
	if dialer == nil {
		dialer = ami.DefaultDialer{}
	}

	labels := func(extra ...string) []string { return extra }

	c := &Collector{
		dialer:  dialer,
		cfg:     cfg,
		options: opts,
		logger:  logger,

		up: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "up"),
			"1 if the last AMI scrape succeeded, 0 otherwise.",
			nil, nil,
		),
		scrapeDuration: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "scrape", "duration_seconds"),
			"Time taken to scrape Asterisk via AMI.",
			nil, nil,
		),
		info: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "info"),
			"Asterisk build information; constant 1.",
			labels("version", "system_name"), nil,
		),
		uptime: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "uptime_seconds"),
			"Seconds since Asterisk core started.",
			nil, nil,
		),
		lastReload: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "last_reload_seconds"),
			"Seconds since the last Asterisk core reload.",
			nil, nil,
		),
		currentCalls: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "current_calls"),
			"Number of currently active calls (CoreStatus).",
			nil, nil,
		),

		channels: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "channels_active"),
			"Number of currently active channels.",
			nil, nil,
		),
		channelsByState: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "channels_by_state"),
			"Active channels grouped by ChannelStateDesc.",
			labels("state"), nil,
		),
		callsActive: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "calls_active"),
			"Active calls deduplicated by Linkedid (one per logical call, regardless of leg count). Use this instead of asterisk_current_calls when 'one phone conversation' should map to '1'.",
			nil, nil,
		),

		sipPeerUp: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "sip", "peer_up"),
			"chan_sip peer reachability (1=OK, 0=otherwise).",
			labels("peer", "status"), nil,
		),
		sipPeerLatencyMs: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "sip", "peer_latency_milliseconds"),
			"chan_sip peer reachability latency in milliseconds (when available).",
			labels("peer"), nil,
		),
		sipPeerCount: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "sip", "peers"),
			"chan_sip peers grouped by status.",
			labels("status"), nil,
		),

		pjsipEndpointUp: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "pjsip", "endpoint_up"),
			"PJSIP endpoint availability (1 if device_state is 'Not in use'/'In use'/'Busy'/'Ringing', 0 otherwise). 'kind' is heuristically 'trunk' or 'extension'.",
			labels("endpoint", "device_state", "kind"), nil,
		),
		pjsipEndpointCount: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "pjsip", "endpoints"),
			"PJSIP endpoints grouped by device_state and heuristic kind (trunk/extension).",
			labels("device_state", "kind"), nil,
		),

		queueCallers: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "queue", "callers"),
			"Callers currently waiting in the queue.",
			labels("queue"), nil,
		),
		queueCompleted: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "queue", "completed_calls"),
			"Calls completed by the queue since startup.",
			labels("queue"), nil,
		),
		queueAbandoned: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "queue", "abandoned_calls"),
			"Calls abandoned in the queue since startup.",
			labels("queue"), nil,
		),
		queueMembers: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "queue", "members"),
			"Queue members grouped by status.",
			labels("queue", "status"), nil,
		),

		scrapeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "scrape",
			Name:      "errors_total",
			Help:      "Total number of AMI scrape failures, by phase.",
		}, []string{"phase"}),
	}
	return c
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	ch <- c.scrapeDuration
	ch <- c.info
	ch <- c.uptime
	ch <- c.lastReload
	ch <- c.currentCalls
	ch <- c.channels
	ch <- c.channelsByState
	ch <- c.callsActive
	ch <- c.sipPeerUp
	ch <- c.sipPeerLatencyMs
	ch <- c.sipPeerCount
	ch <- c.pjsipEndpointUp
	ch <- c.pjsipEndpointCount
	ch <- c.queueCallers
	ch <- c.queueCompleted
	ch <- c.queueAbandoned
	ch <- c.queueMembers
	c.scrapeErrors.Describe(ch)
}

// Collect implements prometheus.Collector. It opens a fresh AMI connection,
// runs all enabled actions, and emits metrics. Errors are reported via the
// up gauge and the scrape_errors_total counter; we never panic.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
	defer cancel()

	scrapeErr := c.scrape(ctx, ch)

	ch <- prometheus.MustNewConstMetric(
		c.scrapeDuration, prometheus.GaugeValue, time.Since(start).Seconds(),
	)
	if scrapeErr != nil {
		c.logger.Error("ami scrape failed", "err", scrapeErr)
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
	} else {
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	}
	c.scrapeErrors.Collect(ch)
}

func (c *Collector) scrape(ctx context.Context, ch chan<- prometheus.Metric) error {
	client, err := c.dialer.Dial(ctx, c.cfg)
	if err != nil {
		c.scrapeErrors.WithLabelValues("dial").Inc()
		return err
	}
	defer client.Close()

	if err := client.Login(ctx); err != nil {
		c.scrapeErrors.WithLabelValues("login").Inc()
		return err
	}
	// Best-effort logoff so the AMI session count doesn't grow.
	defer func() { _ = client.Logoff(ctx) }()

	var firstErr error
	collect := func(phase string, fn func() error) {
		if err := fn(); err != nil {
			c.scrapeErrors.WithLabelValues(phase).Inc()
			c.logger.Warn("ami phase failed", "phase", phase, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	collect("core_status", func() error { return c.collectCore(ctx, client, ch) })
	collect("channels", func() error { return c.collectChannels(ctx, client, ch) })

	if !c.options.DisableSIP {
		collect("sip_peers", func() error { return c.collectSIPPeers(ctx, client, ch) })
	}
	if !c.options.DisablePJSIP {
		collect("pjsip_endpoints", func() error { return c.collectPJSIPEndpoints(ctx, client, ch) })
	}
	if !c.options.DisableQueues {
		collect("queues", func() error { return c.collectQueues(ctx, client, ch) })
	}
	return firstErr
}

