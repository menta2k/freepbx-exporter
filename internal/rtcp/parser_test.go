package rtcp

import (
	"math"
	"testing"

	"github.com/menta2k/freepbx-exporter/internal/ami"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestParseRTCPReceivedModern(t *testing.T) {
	m := ami.NewMessage(
		"Event", "RTCPReceived",
		"ChannelType", "PJSIP",
		"Channel", "PJSIP/100-00000001",
		"IAJitter", "0.012",
		"FractionLost", "0.05",
		"RTT", "0.045",
		"SentPackets", "120",
		"ReceivedPackets", "118",
	)
	s, ok := Parse(m)
	if !ok {
		t.Fatal("Parse returned ok=false")
	}
	if s.Direction != "received" {
		t.Errorf("Direction=%q", s.Direction)
	}
	if s.ChannelType != "PJSIP" {
		t.Errorf("ChannelType=%q", s.ChannelType)
	}
	if !s.HasJitter || !near(s.JitterMS, 12) {
		t.Errorf("JitterMS=%v has=%v", s.JitterMS, s.HasJitter)
	}
	if !s.HasPacketLoss || !near(s.PacketLossRatio, 0.05) {
		t.Errorf("PacketLossRatio=%v has=%v", s.PacketLossRatio, s.HasPacketLoss)
	}
	if !s.HasRTT || !near(s.RTTMS, 45) {
		t.Errorf("RTTMS=%v has=%v", s.RTTMS, s.HasRTT)
	}
	if !s.HasSentPackets || s.SentPackets != 120 {
		t.Errorf("SentPackets=%v has=%v", s.SentPackets, s.HasSentPackets)
	}
}

func TestParseRTCPSentLegacyFields(t *testing.T) {
	// chan_sip-style event: jitter under ReportBlockIAJitter0, fraction
	// lost in 0..255 byte format.
	m := ami.NewMessage(
		"Event", "RTCPSent",
		"ChannelType", "SIP",
		"Channel", "SIP/200-00000002",
		"ReportBlockIAJitter0", "0.005",
		"ReportBlockFractionLost0", "128",
	)
	s, ok := Parse(m)
	if !ok {
		t.Fatal("Parse returned ok=false")
	}
	if s.Direction != "sent" {
		t.Errorf("Direction=%q", s.Direction)
	}
	if !s.HasJitter || !near(s.JitterMS, 5) {
		t.Errorf("JitterMS=%v", s.JitterMS)
	}
	if !s.HasPacketLoss || !near(s.PacketLossRatio, 0.5) {
		t.Errorf("PacketLossRatio=%v want=0.5", s.PacketLossRatio)
	}
}

func TestParseRTCPNonRTCPEventReturnsFalse(t *testing.T) {
	m := ami.NewMessage("Event", "Newchannel", "Channel", "PJSIP/100-1")
	if _, ok := Parse(m); ok {
		t.Error("Parse should reject non-RTCP events")
	}
}

func TestParseRTCPMissingFieldsHaveFalse(t *testing.T) {
	m := ami.NewMessage("Event", "RTCPSent", "Channel", "PJSIP/100-1")
	s, ok := Parse(m)
	if !ok {
		t.Fatal("Parse rejected a valid RTCPSent event")
	}
	if s.HasJitter || s.HasRTT || s.HasPacketLoss || s.HasSentPackets {
		t.Errorf("expected all Has* false, got %+v", s)
	}
}

func TestParseRTCPMalformedNumberSilentlySkipped(t *testing.T) {
	m := ami.NewMessage("Event", "RTCPReceived", "IAJitter", "not-a-number")
	s, _ := Parse(m)
	if s.HasJitter {
		t.Error("HasJitter should be false for malformed input")
	}
}

func TestParseRTCPClampsLossRatio(t *testing.T) {
	m := ami.NewMessage("Event", "RTCPReceived", "FractionLost", "300") // bogus
	s, _ := Parse(m)
	if !s.HasPacketLoss {
		t.Fatal("expected HasPacketLoss")
	}
	if s.PacketLossRatio < 0 || s.PacketLossRatio > 1 {
		t.Errorf("PacketLossRatio out of range: %v", s.PacketLossRatio)
	}
}
