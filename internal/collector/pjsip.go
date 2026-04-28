package collector

import (
	"context"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/menta2k/freepbx-exporter/internal/ami"
)

// pjsipUpStates lists the DeviceState values considered "endpoint reachable"
// for the asterisk_pjsip_endpoint_up gauge. These come from
// ast_devstate2str() in Asterisk source.
var pjsipUpStates = map[string]bool{
	"Not in use": true,
	"In use":     true,
	"Busy":       true,
	"Ringing":    true,
	"Ring":       true,
	"On Hold":    true,
}

func (c *Collector) collectPJSIPEndpoints(ctx context.Context, conn ami.Conn, ch chan<- prometheus.Metric) error {
	items, err := conn.ListAction(ctx,
		ami.NewMessage("Action", "PJSIPShowEndpoints"),
		"EndpointListComplete",
	)
	if err != nil {
		if isUnknownActionErr(err) {
			return nil
		}
		return fmt.Errorf("PJSIPShowEndpoints: %w", err)
	}

	type bucketKey struct{ state, kind string }
	byState := make(map[bucketKey]int, 8)
	for _, m := range items {
		// EndpointList events are the per-endpoint rows; ignore other event
		// types that may sneak in (AOR, AuthList, etc.).
		if !strings.EqualFold(m.Get("Event"), "EndpointList") {
			continue
		}
		endpoint := firstNonEmpty(m.Get("ObjectName"), m.Get("Endpoint"))
		state := firstNonEmpty(m.Get("DeviceState"), "Unknown")
		kind := classifyEndpoint(m)
		byState[bucketKey{state, kind}]++

		up := 0.0
		if pjsipUpStates[state] {
			up = 1.0
		}
		ch <- prometheus.MustNewConstMetric(
			c.pjsipEndpointUp, prometheus.GaugeValue, up, endpoint, state, kind,
		)
	}

	for k, n := range byState {
		ch <- prometheus.MustNewConstMetric(
			c.pjsipEndpointCount, prometheus.GaugeValue, float64(n), k.state, k.kind,
		)
	}
	return nil
}

// classifyEndpoint distinguishes PJSIP trunks from user extensions using the
// fields exposed by EndpointList. Asterisk has no native "trunk" type, so we
// use the strongest signal AMI exposes:
//
//   - OutboundAuths set → endpoint authenticates outbound to a provider → trunk
//   - Auths set, OutboundAuths empty → endpoint accepts inbound auth → extension
//   - both empty → IP-based identification (trunk) if endpoint name is
//     non-numeric (FreePBX convention), otherwise extension
//
// The heuristic isn't bulletproof (you can build any combination in raw
// pjsip.conf) but covers stock FreePBX configurations.
func classifyEndpoint(m ami.Message) string {
	if strings.TrimSpace(m.Get("OutboundAuths")) != "" {
		return "trunk"
	}
	if strings.TrimSpace(m.Get("Auths")) != "" {
		return "extension"
	}
	// No auth on either side: lean on the FreePBX naming convention.
	if isNumericName(m.Get("ObjectName")) {
		return "extension"
	}
	return "trunk"
}

func isNumericName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
