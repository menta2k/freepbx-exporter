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

	byState := make(map[string]int, 8)
	for _, m := range items {
		// EndpointList events are the per-endpoint rows; ignore other event
		// types that may sneak in (AOR, AuthList, etc.).
		if !strings.EqualFold(m.Get("Event"), "EndpointList") {
			continue
		}
		endpoint := firstNonEmpty(m.Get("ObjectName"), m.Get("Endpoint"))
		state := firstNonEmpty(m.Get("DeviceState"), "Unknown")
		byState[state]++

		up := 0.0
		if pjsipUpStates[state] {
			up = 1.0
		}
		ch <- prometheus.MustNewConstMetric(
			c.pjsipEndpointUp, prometheus.GaugeValue, up, endpoint, state,
		)
	}

	for state, n := range byState {
		ch <- prometheus.MustNewConstMetric(
			c.pjsipEndpointCount, prometheus.GaugeValue, float64(n), state,
		)
	}
	return nil
}
