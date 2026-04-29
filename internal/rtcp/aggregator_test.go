package rtcp

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/menta2k/freepbx-exporter/internal/ami"
)

func dump(t *testing.T, c prometheus.Collector) string {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	return buf.String()
}

func TestAggregatorEventsAndJitter(t *testing.T) {
	a := New(Options{})
	ctx := context.Background()
	a.Handle(ctx, ami.NewMessage(
		"Event", "RTCPReceived",
		"ChannelType", "PJSIP",
		"Channel", "PJSIP/100-1",
		"IAJitter", "0.012",
		"FractionLost", "0.10",
	))
	a.Handle(ctx, ami.NewMessage(
		"Event", "RTCPSent",
		"ChannelType", "PJSIP",
		"Channel", "PJSIP/100-1",
		"IAJitter", "0.005",
	))
	out := dump(t, a)

	mustContain := []string{
		`asterisk_rtcp_events_total{direction="received"} 1`,
		`asterisk_rtcp_events_total{direction="sent"} 1`,
		`asterisk_rtcp_jitter_milliseconds_count{channel_type="PJSIP",direction="received"} 1`,
		`asterisk_rtcp_jitter_milliseconds_count{channel_type="PJSIP",direction="sent"} 1`,
		`asterisk_rtcp_packet_loss_ratio{direction="received"} 0.1`,
	}
	for _, w := range mustContain {
		if !strings.Contains(out, w) {
			t.Errorf("missing line: %s", w)
		}
	}
}

func TestAggregatorPerChannelLossLabel(t *testing.T) {
	a := New(Options{PerChannel: true})
	a.Handle(context.Background(), ami.NewMessage(
		"Event", "RTCPReceived",
		"ChannelType", "PJSIP",
		"Channel", "PJSIP/100-1",
		"FractionLost", "0.25",
	))
	out := dump(t, a)
	want := `asterisk_rtcp_packet_loss_ratio{channel="PJSIP/100-1",direction="received"} 0.25`
	if !strings.Contains(out, want) {
		t.Errorf("missing per-channel loss line:\n%s\nwant: %s", out, want)
	}
}

func TestAggregatorOneWayAudioNoInbound(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	tick := func(d time.Duration) func() time.Time { return func() time.Time { return now.Add(d) } }

	a := New(Options{
		MinCallSecondsForOneWay: 5 * time.Second,
		Now:                     tick(0),
	})
	ctx := context.Background()

	// Channel sees outbound RTCPSent only; never receives any peer RTCP.
	a.opts.Now = tick(0)
	a.Handle(ctx, ami.NewMessage("Event", "RTCPSent", "Channel", "PJSIP/200-2", "ChannelType", "PJSIP"))
	a.opts.Now = tick(2 * time.Second)
	a.Handle(ctx, ami.NewMessage("Event", "RTCPSent", "Channel", "PJSIP/200-2"))
	a.opts.Now = tick(20 * time.Second)
	a.Handle(ctx, ami.NewMessage("Event", "Hangup", "Channel", "PJSIP/200-2"))

	out := dump(t, a)
	if !strings.Contains(out, `asterisk_rtcp_one_way_audio_total{reason="no_inbound_rtcp"} 1`) {
		t.Errorf("expected no_inbound_rtcp counter, got:\n%s", out)
	}
}

func TestAggregatorOneWayShortCallSkipped(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	a := New(Options{
		MinCallSecondsForOneWay: 5 * time.Second,
		Now:                     func() time.Time { return now },
	})
	ctx := context.Background()

	a.Handle(ctx, ami.NewMessage("Event", "RTCPSent", "Channel", "PJSIP/300-3"))
	// Hangup at t=0 (same instant) — too short to count.
	a.Handle(ctx, ami.NewMessage("Event", "Hangup", "Channel", "PJSIP/300-3"))

	out := dump(t, a)
	if strings.Contains(out, "asterisk_rtcp_one_way_audio_total") &&
		!strings.Contains(out, "} 0") {
		// Counter should be zero or absent for short calls.
		t.Errorf("short call should not increment one_way counter:\n%s", out)
	}
}

func TestAggregatorStreamUpAndReconnects(t *testing.T) {
	a := New(Options{})
	a.SetStreamUp(true)
	a.IncReconnects()
	a.IncReconnects()
	out := dump(t, a)
	if !strings.Contains(out, "asterisk_event_stream_up 1") {
		t.Errorf("stream_up not 1:\n%s", out)
	}
	if !strings.Contains(out, "asterisk_event_stream_reconnects_total 2") {
		t.Errorf("reconnects_total not 2:\n%s", out)
	}
}

func TestAggregatorChannelTTLPrunesIdle(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	tn := now
	a := New(Options{
		ChannelTTL: 1 * time.Minute,
		Now:        func() time.Time { return tn },
	})
	a.Handle(context.Background(), ami.NewMessage("Event", "RTCPSent", "Channel", "PJSIP/idle-1"))
	if got := func() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.channels) }(); got != 1 {
		t.Fatalf("channel state not stored: %d", got)
	}

	// Advance clock past the TTL and trigger a Collect() to run gc.
	tn = now.Add(2 * time.Minute)
	_ = dump(t, a)

	if got := func() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.channels) }(); got != 0 {
		t.Errorf("expected idle channels to be pruned, %d remain", got)
	}
}
