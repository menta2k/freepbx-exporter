package collector

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/menta2k/freepbx-exporter/internal/ami"
)

// sipLatencyRe captures the latency reported in a chan_sip Status string
// such as "OK (12 ms)".
var sipLatencyRe = regexp.MustCompile(`\((\d+)\s*ms\)`)

func (c *Collector) collectSIPPeers(ctx context.Context, conn ami.Conn, ch chan<- prometheus.Metric) error {
	items, err := conn.ListAction(ctx,
		ami.NewMessage("Action", "SIPpeers"),
		"PeerlistComplete",
	)
	if err != nil {
		// chan_sip is optional in modern Asterisk. If the action is
		// unavailable, downgrade to a soft error so the rest of the scrape
		// keeps going. We detect "No such command" / "Invalid/unknown" by
		// matching common AMI error fragments.
		if isUnknownActionErr(err) {
			return nil
		}
		return fmt.Errorf("SIPpeers: %w", err)
	}

	byStatus := make(map[string]int, 4)
	for _, m := range items {
		peer := m.Get("ObjectName")
		if peer == "" {
			peer = m.Get("Name")
		}
		statusRaw := m.Get("Status")
		statusKey := sipStatusKey(statusRaw)
		byStatus[statusKey]++

		up := 0.0
		if statusKey == "OK" {
			up = 1.0
		}
		ch <- prometheus.MustNewConstMetric(
			c.sipPeerUp, prometheus.GaugeValue, up, peer, statusKey,
		)

		if matches := sipLatencyRe.FindStringSubmatch(statusRaw); len(matches) == 2 {
			if v, err := strconv.ParseFloat(matches[1], 64); err == nil {
				ch <- prometheus.MustNewConstMetric(
					c.sipPeerLatencyMs, prometheus.GaugeValue, v, peer,
				)
			}
		}
	}

	for status, n := range byStatus {
		ch <- prometheus.MustNewConstMetric(
			c.sipPeerCount, prometheus.GaugeValue, float64(n), status,
		)
	}
	return nil
}

// sipStatusKey reduces a free-form Status string to a stable label. Examples:
//   "OK (12 ms)"     -> "OK"
//   "UNREACHABLE"    -> "UNREACHABLE"
//   "LAGGED (50 ms)" -> "LAGGED"
//   ""               -> "UNKNOWN"
func sipStatusKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "UNKNOWN"
	}
	if i := strings.IndexByte(s, ' '); i > 0 {
		return strings.ToUpper(s[:i])
	}
	return strings.ToUpper(s)
}

// isUnknownActionErr matches AMI errors returned when an action is not
// available (e.g. chan_sip not loaded). AMI returns "Response: Error" with
// "Message: Invalid/unknown command" or similar.
func isUnknownActionErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, frag := range []string{
		"invalid/unknown command",
		"no such command",
		"command not registered",
		"unknown action",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}
