// Package rtcp parses RTCP-related AMI events and aggregates them into
// Prometheus metrics.
package rtcp

import (
	"strconv"
	"strings"

	"github.com/menta2k/freepbx-exporter/internal/ami"
)

// Sample is the parsed RTCP measurement extracted from one AMI event.
// All Has* flags indicate whether the corresponding numeric field was
// present in the event; downstream code must skip metric emission when a
// flag is false rather than emit zero.
type Sample struct {
	Direction       string // "sent" if RTCPSent, "received" if RTCPReceived
	ChannelType     string // PJSIP, SIP, IAX2, ""
	Channel         string // e.g. "PJSIP/100-00000001"

	JitterMS        float64
	HasJitter       bool
	RTTMS           float64
	HasRTT          bool
	PacketLossRatio float64 // 0..1
	HasPacketLoss   bool
	SentPackets     uint64
	HasSentPackets  bool
	RecvPackets     uint64
	HasRecvPackets  bool
}

// Parse pulls a Sample out of an AMI event. Returns ok=false for events
// that aren't RTCPSent/RTCPReceived. The parser is tolerant: missing or
// malformed fields are silently dropped (their Has* flags stay false) so
// schema differences between Asterisk versions don't poison the metrics.
func Parse(m ami.Message) (Sample, bool) {
	var s Sample
	switch strings.ToLower(m.Get("Event")) {
	case "rtcpsent":
		s.Direction = "sent"
	case "rtcpreceived":
		s.Direction = "received"
	default:
		return Sample{}, false
	}

	s.ChannelType = m.Get("ChannelType")
	s.Channel = firstNonEmpty(m.Get("Channel"), m.Get("ChannelObjectName"))

	// Jitter — modern field then a few legacy variants.
	for _, key := range []string{
		"IAJitter",
		"ReportBlockIAJitter0",
		"ReportBlock0IAJitter",
		"Jitter",
	} {
		if v := m.Get(key); v != "" {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				// Asterisk reports jitter in seconds (float). Convert to ms.
				// Some legacy variants emit ms directly — values >1 are almost
				// certainly milliseconds already.
				if f > 1 {
					s.JitterMS = f
				} else {
					s.JitterMS = f * 1000
				}
				s.HasJitter = true
				break
			}
		}
	}

	// Packet loss — fraction is 0..1 in modern events; legacy chan_sip
	// emits 0..255 (the raw RTCP byte).
	for _, key := range []string{
		"FractionLost",
		"ReportBlockFractionLost0",
		"ReportBlock0FractionLost",
	} {
		if v := m.Get(key); v != "" {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				if f > 1 {
					f /= 256
				}
				if f < 0 {
					f = 0
				}
				if f > 1 {
					f = 1
				}
				s.PacketLossRatio = f
				s.HasPacketLoss = true
				break
			}
		}
	}

	// Round-trip time — Asterisk PJSIP exports this; chan_sip rarely does.
	for _, key := range []string{"RTT", "RoundTripTime"} {
		if v := m.Get(key); v != "" {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				// RTT comes through as either seconds (float) or
				// milliseconds. Heuristic: small values are seconds.
				if f > 0 && f < 10 {
					f *= 1000
				}
				if f > 0 {
					s.RTTMS = f
					s.HasRTT = true
					break
				}
			}
		}
	}

	if v := m.Get("SentPackets"); v != "" {
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			s.SentPackets = n
			s.HasSentPackets = true
		}
	}
	if v := m.Get("ReceivedPackets"); v != "" {
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			s.RecvPackets = n
			s.HasRecvPackets = true
		}
	}

	return s, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
