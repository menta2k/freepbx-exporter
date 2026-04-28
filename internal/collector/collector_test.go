package collector

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"

	"github.com/menta2k/freepbx-exporter/internal/ami"
)

// fakeConn is an in-memory ami.Conn used to drive the collector without a
// real AMI server.
type fakeConn struct {
	actions  map[string]ami.Message
	lists    map[string][]ami.Message
	loginErr error
	dialErr  error
	closed   bool
}

func (f *fakeConn) Login(ctx context.Context) error  { return f.loginErr }
func (f *fakeConn) Logoff(ctx context.Context) error { return nil }
func (f *fakeConn) Close() error                     { f.closed = true; return nil }

func (f *fakeConn) Action(ctx context.Context, req ami.Message) (ami.Message, error) {
	name := req.Get("Action")
	if msg, ok := f.actions[name]; ok {
		return msg, nil
	}
	return ami.Message{}, errors.New("action " + name + " not scripted")
}

func (f *fakeConn) ListAction(ctx context.Context, req ami.Message, _ string) ([]ami.Message, error) {
	name := req.Get("Action")
	if items, ok := f.lists[name]; ok {
		return items, nil
	}
	return nil, errors.New("invalid/unknown command: " + name)
}

type fakeDialer struct{ conn *fakeConn }

func (d fakeDialer) Dial(ctx context.Context, _ ami.Config) (ami.Conn, error) {
	if d.conn.dialErr != nil {
		return nil, d.conn.dialErr
	}
	return d.conn, nil
}

func newFixtureCollector(t *testing.T, conn *fakeConn, opts ScrapeOptions) *Collector {
	t.Helper()
	cfg := ami.Config{Address: "127.0.0.1:5038", Username: "u", Secret: "p"}
	return New(cfg, opts, nil, fakeDialer{conn: conn})
}

func TestCollectorHappyPath(t *testing.T) {
	conn := &fakeConn{
		actions: map[string]ami.Message{
			"CoreSettings": ami.NewMessage(
				"Response", "Success",
				"AsteriskVersion", "18.20.0",
				"SystemName", "pbx-1",
			),
			"CoreStatus": ami.NewMessage(
				"Response", "Success",
				"CoreCurrentCalls", "3",
			),
		},
		lists: map[string][]ami.Message{
			"CoreShowChannels": {
				ami.NewMessage("Event", "CoreShowChannel", "Channel", "PJSIP/100-001", "ChannelStateDesc", "Up"),
				ami.NewMessage("Event", "CoreShowChannel", "Channel", "PJSIP/101-002", "ChannelStateDesc", "Ringing"),
				ami.NewMessage("Event", "CoreShowChannel", "Channel", "PJSIP/102-003", "ChannelStateDesc", "Up"),
			},
			"SIPpeers": {
				ami.NewMessage("Event", "PeerEntry", "ObjectName", "100", "Status", "OK (12 ms)"),
				ami.NewMessage("Event", "PeerEntry", "ObjectName", "101", "Status", "UNREACHABLE"),
				ami.NewMessage("Event", "PeerEntry", "ObjectName", "102", "Status", "LAGGED (250 ms)"),
			},
			"PJSIPShowEndpoints": {
				// Numeric name + inbound Auths -> extension.
				ami.NewMessage("Event", "EndpointList",
					"ObjectName", "200", "DeviceState", "Not in use",
					"Auths", "auth200"),
				ami.NewMessage("Event", "EndpointList",
					"ObjectName", "201", "DeviceState", "Unavailable",
					"Auths", "auth201"),
				// OutboundAuths -> trunk.
				ami.NewMessage("Event", "EndpointList",
					"ObjectName", "ITD", "DeviceState", "Not in use",
					"OutboundAuths", "itd-outbound"),
				// No auths, non-numeric name -> trunk by naming heuristic.
				ami.NewMessage("Event", "EndpointList",
					"ObjectName", "Kamailio", "DeviceState", "Not in use"),
				ami.NewMessage("Event", "ContactStatusDetail", "ObjectName", "ignored"), // must be skipped
			},
			"QueueStatus": {
				ami.NewMessage("Event", "QueueParams",
					"Queue", "support",
					"Calls", "2",
					"Completed", "100",
					"Abandoned", "5",
				),
				ami.NewMessage("Event", "QueueMember", "Queue", "support", "Status", "1"),
				ami.NewMessage("Event", "QueueMember", "Queue", "support", "Status", "2"),
				ami.NewMessage("Event", "QueueMember", "Queue", "support", "Status", "5"),
			},
		},
	}

	c := newFixtureCollector(t, conn, ScrapeOptions{})

	if got := testutil.ToFloat64(metricByDesc(t, c, c.up)); got != 1 {
		t.Errorf("up = %v, want 1", got)
	}

	expected := strings.NewReader(`
# HELP asterisk_channels_active Number of currently active channels.
# TYPE asterisk_channels_active gauge
asterisk_channels_active 3
# HELP asterisk_current_calls Number of currently active calls (CoreStatus).
# TYPE asterisk_current_calls gauge
asterisk_current_calls 3
# HELP asterisk_info Asterisk build information; constant 1.
# TYPE asterisk_info gauge
asterisk_info{system_name="pbx-1",version="18.20.0"} 1
`)
	if err := testutil.CollectAndCompare(c, expected,
		"asterisk_channels_active",
		"asterisk_current_calls",
		"asterisk_info",
	); err != nil {
		t.Errorf("CollectAndCompare:\n%v", err)
	}

	// Spot-check a few high-cardinality metrics.
	mustHave := []string{
		`asterisk_channels_by_state{state="Up"} 2`,
		`asterisk_channels_by_state{state="Ringing"} 1`,
		`asterisk_sip_peer_up{peer="100",status="OK"} 1`,
		`asterisk_sip_peer_up{peer="101",status="UNREACHABLE"} 0`,
		`asterisk_sip_peer_latency_milliseconds{peer="100"} 12`,
		`asterisk_sip_peer_latency_milliseconds{peer="102"} 250`,
		`asterisk_pjsip_endpoint_up{device_state="Not in use",endpoint="200",kind="extension"} 1`,
		`asterisk_pjsip_endpoint_up{device_state="Unavailable",endpoint="201",kind="extension"} 0`,
		`asterisk_pjsip_endpoint_up{device_state="Not in use",endpoint="ITD",kind="trunk"} 1`,
		`asterisk_pjsip_endpoint_up{device_state="Not in use",endpoint="Kamailio",kind="trunk"} 1`,
		`asterisk_queue_callers{queue="support"} 2`,
		`asterisk_queue_completed_calls{queue="support"} 100`,
		`asterisk_queue_abandoned_calls{queue="support"} 5`,
		`asterisk_queue_members{queue="support",status="not_in_use"} 1`,
		`asterisk_queue_members{queue="support",status="in_use"} 1`,
		`asterisk_queue_members{queue="support",status="unavailable"} 1`,
	}
	dump := dumpRegistry(t, c)
	for _, want := range mustHave {
		if !strings.Contains(dump, want) {
			t.Errorf("missing metric line: %s", want)
		}
	}
}

func TestCollectorDialFailureSetsUpZero(t *testing.T) {
	conn := &fakeConn{dialErr: errors.New("connection refused")}
	c := newFixtureCollector(t, conn, ScrapeOptions{})

	dump := dumpRegistry(t, c)
	if !strings.Contains(dump, "asterisk_up 0") {
		t.Errorf("expected asterisk_up 0, got:\n%s", dump)
	}
	if !strings.Contains(dump, `asterisk_scrape_errors_total{phase="dial"} 1`) {
		t.Errorf("expected dial error counter, got:\n%s", dump)
	}
}

func TestCollectorLoginFailureSetsUpZero(t *testing.T) {
	conn := &fakeConn{loginErr: errors.New("Authentication failed")}
	c := newFixtureCollector(t, conn, ScrapeOptions{})

	dump := dumpRegistry(t, c)
	if !strings.Contains(dump, "asterisk_up 0") {
		t.Errorf("expected asterisk_up 0, got:\n%s", dump)
	}
	if !strings.Contains(dump, `asterisk_scrape_errors_total{phase="login"} 1`) {
		t.Errorf("expected login error counter")
	}
}

func TestCollectorMissingChanSIP(t *testing.T) {
	conn := &fakeConn{
		actions: map[string]ami.Message{
			"CoreSettings": ami.NewMessage("Response", "Success", "AsteriskVersion", "20.0.0"),
			"CoreStatus":   ami.NewMessage("Response", "Success", "CoreCurrentCalls", "0"),
		},
		// SIPpeers, PJSIPShowEndpoints, QueueStatus, CoreShowChannels deliberately missing.
		// The fake returns "invalid/unknown command" which should be treated as soft-skip.
		lists: map[string][]ami.Message{},
	}
	c := newFixtureCollector(t, conn, ScrapeOptions{})

	dump := dumpRegistry(t, c)
	// CoreShowChannels failure is *not* soft-skipped — that should still flip up=0,
	// but core/peers/pjsip/queues errors of "unknown action" type for the optional
	// modules should not. Verify channels phase fails:
	if !strings.Contains(dump, `asterisk_scrape_errors_total{phase="channels"}`) {
		t.Errorf("expected channels phase to be reported as error, got:\n%s", dump)
	}
}

// metricByDesc finds the gauge value for a single-valued descriptor by
// running the collector and matching on Desc(). Returns a nilable Collector
// that reports the value to testutil.ToFloat64.
func metricByDesc(t *testing.T, c *Collector, desc *prometheus.Desc) prometheus.Collector {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	wantName := strings.SplitN(strings.SplitN(desc.String(), `"`, 3)[1], `"`, 2)[0]
	for _, mf := range mfs {
		if mf.GetName() == wantName {
			g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_proxy"})
			g.Set(mf.Metric[0].Gauge.GetValue())
			return g
		}
	}
	t.Fatalf("metric %q not produced", wantName)
	return nil
}

func dumpRegistry(t *testing.T, c *Collector) string {
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
