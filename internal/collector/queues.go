package collector

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/menta2k/freepbx-exporter/internal/ami"
)

// QueueStatus emits three event types interleaved (terminated by
// QueueStatusComplete):
//
//   - QueueParams: per-queue summary (Queue, Calls, Completed, Abandoned,...)
//   - QueueMember: per-member row    (Queue, Name, Status, ...)
//   - QueueEntry:  per-caller row    (Queue, Position, ...)
//
// The action is available only when app_queue is loaded; if not, we silently
// skip per the isUnknownActionErr branch.
func (c *Collector) collectQueues(ctx context.Context, conn ami.Conn, ch chan<- prometheus.Metric) error {
	items, err := conn.ListAction(ctx,
		ami.NewMessage("Action", "QueueStatus"),
		"QueueStatusComplete",
	)
	if err != nil {
		if isUnknownActionErr(err) {
			return nil
		}
		return fmt.Errorf("QueueStatus: %w", err)
	}

	// queue -> status -> count
	memberCounts := make(map[string]map[string]int)

	for _, m := range items {
		switch strings.ToLower(m.Get("Event")) {
		case "queueparams":
			queue := m.Get("Queue")
			if queue == "" {
				continue
			}
			emitFloat(ch, c.queueCallers, prometheus.GaugeValue, m.Get("Calls"), queue)
			emitFloat(ch, c.queueCompleted, prometheus.CounterValue, m.Get("Completed"), queue)
			emitFloat(ch, c.queueAbandoned, prometheus.CounterValue, m.Get("Abandoned"), queue)
		case "queuemember":
			queue := m.Get("Queue")
			status := queueMemberStatus(m.Get("Status"))
			if queue == "" {
				continue
			}
			if _, ok := memberCounts[queue]; !ok {
				memberCounts[queue] = make(map[string]int, 4)
			}
			memberCounts[queue][status]++
		}
	}

	for queue, byStatus := range memberCounts {
		for status, n := range byStatus {
			ch <- prometheus.MustNewConstMetric(
				c.queueMembers, prometheus.GaugeValue, float64(n), queue, status,
			)
		}
	}
	return nil
}

// queueMemberStatus maps the numeric Status field returned by app_queue to a
// human label. See app_queue.c AST_DEVICE_* constants.
func queueMemberStatus(s string) string {
	switch strings.TrimSpace(s) {
	case "0":
		return "unknown"
	case "1":
		return "not_in_use"
	case "2":
		return "in_use"
	case "3":
		return "busy"
	case "4":
		return "invalid"
	case "5":
		return "unavailable"
	case "6":
		return "ringing"
	case "7":
		return "ringinuse"
	case "8":
		return "on_hold"
	case "":
		return "unknown"
	default:
		return s
	}
}

// emitFloat parses s as a float and emits a metric. Non-numeric or empty
// values are silently skipped — Asterisk omits some fields conditionally.
func emitFloat(ch chan<- prometheus.Metric, desc *prometheus.Desc, vt prometheus.ValueType, s string, labels ...string) {
	if s == "" {
		return
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, vt, v, labels...)
}
