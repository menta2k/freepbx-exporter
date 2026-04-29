package rtcp

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/menta2k/freepbx-exporter/internal/ami"
)

// Options tune the aggregator. Zero value is sensible.
type Options struct {
	// PerChannel emits asterisk_rtcp_packet_loss_ratio with a "channel"
	// label, useful for short-lived debugging but high cardinality.
	PerChannel bool

	// MinCallSecondsForOneWay is the floor below which a hangup with zero
	// RTCPReceived events does NOT count as one-way audio (too short to
	// distinguish from a normal call setup). Default: 5s.
	MinCallSecondsForOneWay time.Duration

	// ChannelTTL bounds how long a channel without any further events
	// stays in the aggregator's in-memory state. Default: 1 hour.
	ChannelTTL time.Duration

	// Now is injected for tests; defaults to time.Now.
	Now func() time.Time
}

// Aggregator consumes RTCP/Hangup AMI events on the write side and serves
// Prometheus metrics on the read side. It is safe for concurrent use.
//
// Implements both ami.EventHandler and prometheus.Collector.
type Aggregator struct {
	opts Options

	jitter      *prometheus.HistogramVec
	rtt         *prometheus.HistogramVec
	loss        *prometheus.GaugeVec
	events      *prometheus.CounterVec
	oneWay      *prometheus.CounterVec
	streamUp    prometheus.Gauge
	reconnects  prometheus.Counter

	mu       sync.Mutex
	channels map[string]*chanState
}

type chanState struct {
	firstSeen     time.Time
	lastSeen      time.Time
	rtcpSent      uint64 // RTCPSent events for this channel
	rtcpReceived  uint64 // RTCPReceived events for this channel
}

// New constructs an Aggregator with metric descriptors registered against
// no registry yet — caller must reg.Register(agg).
func New(opts Options) *Aggregator {
	if opts.MinCallSecondsForOneWay == 0 {
		opts.MinCallSecondsForOneWay = 5 * time.Second
	}
	if opts.ChannelTTL == 0 {
		opts.ChannelTTL = 1 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	a := &Aggregator{
		opts:     opts,
		channels: make(map[string]*chanState),

		jitter: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "asterisk",
			Subsystem: "rtcp",
			Name:      "jitter_milliseconds",
			Help:      "Inter-arrival jitter reported in RTCP events (ms).",
			Buckets:   []float64{1, 2.5, 5, 10, 20, 40, 80, 160, 320},
		}, []string{"direction", "channel_type"}),

		rtt: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "asterisk",
			Subsystem: "rtcp",
			Name:      "rtt_milliseconds",
			Help:      "Round-trip time reported in RTCP events (ms). Only emitted when the AMI event includes RTT.",
			Buckets:   []float64{5, 10, 20, 40, 80, 160, 320, 640, 1280},
		}, []string{"channel_type"}),

		// loss is per-channel only when PerChannel is set, otherwise we
		// keep a single 'aggregate' time-series. The label set is
		// determined at construction time.
		loss: newLossVec(opts.PerChannel),

		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "asterisk",
			Subsystem: "rtcp",
			Name:      "events_total",
			Help:      "RTCP events observed on the AMI stream, by direction.",
		}, []string{"direction"}),

		oneWay: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "asterisk",
			Subsystem: "rtcp",
			Name:      "one_way_audio_total",
			Help:      "Channels that ended showing one-way RTCP flow (heuristic). 'reason' is no_inbound_rtcp or no_outbound_rtcp.",
		}, []string{"reason"}),

		streamUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "asterisk",
			Subsystem: "event_stream",
			Name:      "up",
			Help:      "1 if the persistent AMI event-stream connection is healthy.",
		}),

		reconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "asterisk",
			Subsystem: "event_stream",
			Name:      "reconnects_total",
			Help:      "Total number of AMI event-stream reconnect attempts.",
		}),
	}
	return a
}

func newLossVec(perChannel bool) *prometheus.GaugeVec {
	opts := prometheus.GaugeOpts{
		Namespace: "asterisk",
		Subsystem: "rtcp",
		Name:      "packet_loss_ratio",
		Help:      "Last-seen RTCP fraction-lost (0..1). When --rtcp-per-channel is true, labeled per channel.",
	}
	if perChannel {
		return prometheus.NewGaugeVec(opts, []string{"channel", "direction"})
	}
	return prometheus.NewGaugeVec(opts, []string{"direction"})
}

// SetStreamUp updates the asterisk_event_stream_up gauge.
func (a *Aggregator) SetStreamUp(up bool) {
	if up {
		a.streamUp.Set(1)
	} else {
		a.streamUp.Set(0)
	}
}

// IncReconnects bumps the reconnect counter.
func (a *Aggregator) IncReconnects() { a.reconnects.Inc() }

// Describe implements prometheus.Collector.
func (a *Aggregator) Describe(ch chan<- *prometheus.Desc) {
	a.jitter.Describe(ch)
	a.rtt.Describe(ch)
	a.loss.Describe(ch)
	a.events.Describe(ch)
	a.oneWay.Describe(ch)
	ch <- a.streamUp.Desc()
	ch <- a.reconnects.Desc()
}

// Collect implements prometheus.Collector.
func (a *Aggregator) Collect(ch chan<- prometheus.Metric) {
	a.jitter.Collect(ch)
	a.rtt.Collect(ch)
	a.loss.Collect(ch)
	a.events.Collect(ch)
	a.oneWay.Collect(ch)
	ch <- a.streamUp
	ch <- a.reconnects
	a.gcStaleChannels()
}

// Handle dispatches an AMI event. Implements ami.EventHandler.
func (a *Aggregator) Handle(_ context.Context, m ami.Message) {
	switch strings.ToLower(m.Get("Event")) {
	case "rtcpsent", "rtcpreceived":
		a.handleRTCP(m)
	case "hangup":
		a.handleHangup(m)
	}
}

func (a *Aggregator) handleRTCP(m ami.Message) {
	s, ok := Parse(m)
	if !ok {
		return
	}
	a.events.WithLabelValues(s.Direction).Inc()

	if s.HasJitter {
		ct := s.ChannelType
		if ct == "" {
			ct = "unknown"
		}
		a.jitter.WithLabelValues(s.Direction, ct).Observe(s.JitterMS)
	}
	if s.HasRTT {
		ct := s.ChannelType
		if ct == "" {
			ct = "unknown"
		}
		a.rtt.WithLabelValues(ct).Observe(s.RTTMS)
	}
	if s.HasPacketLoss {
		if a.opts.PerChannel {
			a.loss.WithLabelValues(s.Channel, s.Direction).Set(s.PacketLossRatio)
		} else {
			a.loss.WithLabelValues(s.Direction).Set(s.PacketLossRatio)
		}
	}

	// Per-channel state for one-way detection, only when we have a name.
	if s.Channel == "" {
		return
	}
	now := a.opts.Now()
	a.mu.Lock()
	st, ok := a.channels[s.Channel]
	if !ok {
		st = &chanState{firstSeen: now}
		a.channels[s.Channel] = st
	}
	st.lastSeen = now
	switch s.Direction {
	case "sent":
		st.rtcpSent++
	case "received":
		st.rtcpReceived++
	}
	a.mu.Unlock()
}

func (a *Aggregator) handleHangup(m ami.Message) {
	chanName := firstNonEmpty(m.Get("Channel"), m.Get("ChannelObjectName"))
	if chanName == "" {
		return
	}
	now := a.opts.Now()

	a.mu.Lock()
	st, ok := a.channels[chanName]
	if ok {
		delete(a.channels, chanName)
	}
	a.mu.Unlock()
	if !ok {
		return // call we never saw RTCP for; nothing to assert
	}

	if now.Sub(st.firstSeen) < a.opts.MinCallSecondsForOneWay {
		return // too short to draw conclusions
	}
	switch {
	case st.rtcpSent > 0 && st.rtcpReceived == 0:
		// We sent RTCP but never received any from peer — peer not
		// responding (often = caller hears nothing).
		a.oneWay.WithLabelValues("no_inbound_rtcp").Inc()
	case st.rtcpReceived > 0 && st.rtcpSent == 0:
		a.oneWay.WithLabelValues("no_outbound_rtcp").Inc()
	}
}

// gcStaleChannels prunes channel entries that haven't seen events past the
// TTL. Called from Collect so it amortizes cleanup across scrapes.
func (a *Aggregator) gcStaleChannels() {
	cutoff := a.opts.Now().Add(-a.opts.ChannelTTL)
	a.mu.Lock()
	for k, st := range a.channels {
		if st.lastSeen.Before(cutoff) {
			delete(a.channels, k)
		}
	}
	a.mu.Unlock()
}
