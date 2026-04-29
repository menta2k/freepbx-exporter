package collector

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/menta2k/freepbx-exporter/internal/ami"
)

// asteriskTimeFormat is the layout returned by CoreStatus for CoreStartupTime
// and CoreReloadTime ("HH:MM:SS"); CoreStartupDate is "YYYY-MM-DD".
const (
	asteriskDate = "2006-01-02"
	asteriskTime = "15:04:05"
)

func (c *Collector) collectCore(ctx context.Context, conn ami.Conn, ch chan<- prometheus.Metric) error {
	settings, err := conn.Action(ctx, ami.NewMessage("Action", "CoreSettings"))
	if err != nil {
		return fmt.Errorf("CoreSettings: %w", err)
	}
	status, err := conn.Action(ctx, ami.NewMessage("Action", "CoreStatus"))
	if err != nil {
		return fmt.Errorf("CoreStatus: %w", err)
	}

	version := firstNonEmpty(settings.Get("AsteriskVersion"), settings.Get("Version"))
	systemName := settings.Get("SystemName")
	ch <- prometheus.MustNewConstMetric(
		c.info, prometheus.GaugeValue, 1, version, systemName,
	)

	// CoreCurrentCalls is a stringy integer.
	if v := status.Get("CoreCurrentCalls"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			ch <- prometheus.MustNewConstMetric(c.currentCalls, prometheus.GaugeValue, n)
		}
	}

	// Uptime is computed from CoreStartupDate + CoreStartupTime when present.
	if t, ok := parseAsteriskMoment(status.Get("CoreStartupDate"), status.Get("CoreStartupTime")); ok {
		ch <- prometheus.MustNewConstMetric(
			c.uptime, prometheus.GaugeValue, time.Since(t).Seconds(),
		)
	}
	if t, ok := parseAsteriskMoment(status.Get("CoreReloadDate"), status.Get("CoreReloadTime")); ok {
		ch <- prometheus.MustNewConstMetric(
			c.lastReload, prometheus.GaugeValue, time.Since(t).Seconds(),
		)
	}
	return nil
}

func (c *Collector) collectChannels(ctx context.Context, conn ami.Conn, ch chan<- prometheus.Metric) error {
	items, err := conn.ListAction(ctx,
		ami.NewMessage("Action", "CoreShowChannels"),
		"CoreShowChannelsComplete",
	)
	if err != nil {
		return fmt.Errorf("CoreShowChannels: %w", err)
	}

	ch <- prometheus.MustNewConstMetric(
		c.channels, prometheus.GaugeValue, float64(len(items)),
	)

	byState := make(map[string]int, 8)
	// Each unique Linkedid is one logical call, regardless of leg count
	// (a 2-party PJSIP call has 2 channels but 1 Linkedid; a 3-way
	// conference has 3 channels and still 1 Linkedid). Channels without
	// a Linkedid (rare; pre-bridge originate dialplan code) fall back to
	// Uniqueid so they aren't all collapsed into the empty bucket.
	linkedids := make(map[string]struct{}, len(items))
	for _, m := range items {
		state := firstNonEmpty(m.Get("ChannelStateDesc"), m.Get("ChannelState"), "Unknown")
		byState[state]++
		id := firstNonEmpty(m.Get("Linkedid"), m.Get("Uniqueid"))
		if id != "" {
			linkedids[id] = struct{}{}
		}
	}
	for state, n := range byState {
		ch <- prometheus.MustNewConstMetric(
			c.channelsByState, prometheus.GaugeValue, float64(n), state,
		)
	}
	ch <- prometheus.MustNewConstMetric(
		c.callsActive, prometheus.GaugeValue, float64(len(linkedids)),
	)
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseAsteriskMoment combines a CoreStartupDate ("YYYY-MM-DD") and
// CoreStartupTime ("HH:MM:SS") into a time.Time in the local timezone.
func parseAsteriskMoment(date, t string) (time.Time, bool) {
	date = strings.TrimSpace(date)
	t = strings.TrimSpace(t)
	if date == "" || t == "" {
		return time.Time{}, false
	}
	full, err := time.ParseInLocation(asteriskDate+" "+asteriskTime, date+" "+t, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return full, true
}
