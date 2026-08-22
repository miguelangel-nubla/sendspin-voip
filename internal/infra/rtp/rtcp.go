package rtp

import (
	"encoding/binary"
	"fmt"
	"time"
)

// NTP epoch offset in seconds (from Jan 1, 1900 to Jan 1, 1970).
const ntpEpochOffset = 2208988800

// TimeToNTP converts a time.Time to NTP 64-bit timestamp (32-bit seconds, 32-bit fraction).
func TimeToNTP(t time.Time) (sec uint32, frac uint32) {
	unixSec := t.Unix()
	sec = uint32(unixSec + ntpEpochOffset)
	frac = uint32(uint64(t.Nanosecond()) * (1 << 32) / 1e9)
	return sec, frac
}

// BuildRTCPSenderReport generates an RFC 3550 RTCP Sender Report (SR) packet.
func BuildRTCPSenderReport(ssrc uint32, rtpTimestamp uint32, packetCount uint32, octetCount uint32, now time.Time) []byte {
	buf := make([]byte, 28)
	// V=2, P=0, RC=0
	buf[0] = 0x80
	// PT = 200 (SR)
	buf[1] = 200
	// Length in 32-bit words minus 1 = 6
	binary.BigEndian.PutUint16(buf[2:4], 6)

	// Sender SSRC
	binary.BigEndian.PutUint32(buf[4:8], ssrc)

	// NTP Timestamp
	ntpSec, ntpFrac := TimeToNTP(now)
	binary.BigEndian.PutUint32(buf[8:12], ntpSec)
	binary.BigEndian.PutUint32(buf[12:16], ntpFrac)

	// RTP Timestamp
	binary.BigEndian.PutUint32(buf[16:20], rtpTimestamp)

	// Packet count
	binary.BigEndian.PutUint32(buf[20:24], packetCount)

	// Octet count
	binary.BigEndian.PutUint32(buf[24:28], octetCount)

	return buf
}

// RTCPReceiverReportData holds parsed statistics from an RTCP Receiver Report (RR).
type RTCPReceiverReportData struct {
	SSRC         uint32
	FractionLost float64 // 0.0 to 100.0 percent
	PacketsLost  int32
	HighestSeq   uint32
	Jitter       uint32
	LSR          uint32
	DLSR         uint32
}

// ParseRTCPReceiverReport parses an incoming RTCP packet buffer (PT=201 Receiver Report or PT=200 SR with RR blocks).
func ParseRTCPReceiverReport(data []byte) (*RTCPReceiverReportData, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short for RTCP header")
	}

	version := (data[0] >> 6) & 0x03
	if version != 2 {
		return nil, fmt.Errorf("unsupported RTCP version %d", version)
	}

	pt := data[1]
	rc := data[0] & 0x1F

	// PT=201 is Receiver Report (RR); PT=200 is Sender Report (SR)
	var offset int
	if pt == 201 {
		// RR header is 8 bytes
		offset = 8
	} else if pt == 200 {
		// SR header + sender info is 28 bytes before report blocks
		offset = 28
	} else {
		return nil, fmt.Errorf("unsupported RTCP packet type %d", pt)
	}

	if rc == 0 || len(data) < offset+24 {
		return nil, nil // No report block present
	}

	block := data[offset : offset+24]
	sourceSSRC := binary.BigEndian.Uint32(block[0:4])
	fractionLostRaw := block[4]
	fractionLostPct := (float64(fractionLostRaw) / 256.0) * 100.0

	// Cumulative packet loss (24-bit signed)
	lostRaw := uint32(block[5])<<16 | uint32(block[6])<<8 | uint32(block[7])
	var lost int32
	if lostRaw&0x800000 != 0 {
		lost = int32(lostRaw | 0xFF000000)
	} else {
		lost = int32(lostRaw)
	}

	highestSeq := binary.BigEndian.Uint32(block[8:12])
	jitter := binary.BigEndian.Uint32(block[12:16])
	lsr := binary.BigEndian.Uint32(block[16:20])
	dlsr := binary.BigEndian.Uint32(block[20:24])

	return &RTCPReceiverReportData{
		SSRC:         sourceSSRC,
		FractionLost: fractionLostPct,
		PacketsLost:  lost,
		HighestSeq:   highestSeq,
		Jitter:       jitter,
		LSR:          lsr,
		DLSR:         dlsr,
	}, nil
}
