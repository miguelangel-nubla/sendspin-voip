package sip

import (
	"strings"
	"testing"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

func TestBuildAndParseSDP(t *testing.T) {
	offer, err := BuildSDPOffer("192.168.1.100", 10002, domain.CodecG722)
	if err != nil {
		t.Fatalf("BuildSDPOffer failed: %v", err)
	}

	if !strings.Contains(offer, "m=audio 10002 RTP/AVP 9") {
		t.Errorf("expected m=audio line in offer, got:\n%s", offer)
	}
	if !strings.Contains(offer, "a=rtpmap:9 G722/8000") {
		t.Errorf("expected rtpmap in offer, got:\n%s", offer)
	}

	// Mock Remote 200 OK SDP Answer
	remoteSDP := `v=0
o=phone 987654 1 IN IP4 192.168.1.105
s=SIP Call
c=IN IP4 192.168.1.105
t=0 0
m=audio 16384 RTP/AVP 9
a=rtpmap:9 G722/8000
a=sendrecv
`

	remoteUDP, negotiatedCodec, err := ParseRemoteSDP(remoteSDP, "192.168.1.105")
	if err != nil {
		t.Fatalf("ParseRemoteSDP failed: %v", err)
	}

	if remoteUDP.IP.String() != "192.168.1.105" || remoteUDP.Port != 16384 {
		t.Errorf("expected remote UDP 192.168.1.105:16384, got %s", remoteUDP.String())
	}
	if negotiatedCodec != domain.CodecG722 {
		t.Errorf("expected CodecG722, got %s", negotiatedCodec)
	}

	// Test ParseSDPCodecs
	multiSDP := `v=0
o=- 123 1 IN IP4 10.2.3.238
s=-
c=IN IP4 10.2.3.238
t=0 0
m=audio 19882 RTP/AVP 96 97 9 0 8
a=rtpmap:96 opus/48000/2
a=rtpmap:97 L16/48000/1
a=rtpmap:9 G722/8000
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
`
	codecs := ParseSDPCodecs(multiSDP)
	if len(codecs) != 5 {
		t.Fatalf("expected 5 codecs parsed, got %d: %v", len(codecs), codecs)
	}
	if codecs[0] != domain.CodecOpus || codecs[1] != domain.CodecL16 || codecs[2] != domain.CodecG722 || codecs[3] != domain.CodecPCMU || codecs[4] != domain.CodecPCMA {
		t.Errorf("unexpected codecs order: %v", codecs)
	}

	// Dynamic PT without rtpmap must not be guessed as Opus
	bare := `v=0
o=- 1 1 IN IP4 10.0.0.1
s=-
c=IN IP4 10.0.0.1
t=0 0
m=audio 10000 RTP/AVP 96 9 0
`
	bareCodecs := ParseSDPCodecs(bare)
	if len(bareCodecs) != 2 || bareCodecs[0] != domain.CodecG722 || bareCodecs[1] != domain.CodecPCMU {
		t.Fatalf("expected only static PTs [g722 pcmu], got %v", bareCodecs)
	}

	offerL16, err := BuildSDPOffer("10.0.0.1", 10004, domain.CodecL16)
	if err != nil {
		t.Fatalf("BuildSDPOffer L16: %v", err)
	}
	if !strings.Contains(offerL16, "L16/48000/1") {
		t.Errorf("expected mono L16 rtpmap, got:\n%s", offerL16)
	}
}

// TestParseSDPCodecs_DoesNotConflateG722WithG7221 pins the codec-name matching.
// rtpmap names used to be matched by prefix, so "G7221" and "G722.1" — G.722.1,
// a different codec at a different bitrate — were both reported as plain G.722.
// The bridge would then encode G.722 for a phone that never offered it.
func TestParseSDPCodecs_DoesNotConflateG722WithG7221(t *testing.T) {
	sdpBody := "v=0\r\n" +
		"o=- 1 1 IN IP4 192.168.1.60\r\n" +
		"s=-\r\n" +
		"c=IN IP4 192.168.1.60\r\n" +
		"t=0 0\r\n" +
		"m=audio 5004 RTP/AVP 100 0\r\n" +
		"a=rtpmap:100 G7221/16000\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n"

	codecs := ParseSDPCodecs(sdpBody)

	for _, c := range codecs {
		if c == domain.CodecG722 {
			t.Fatalf("G7221 (G.722.1) was reported as G.722; got codecs %v", codecs)
		}
	}
	if len(codecs) != 1 || codecs[0] != domain.CodecPCMU {
		t.Errorf("expected only PCMU to be recognised, got %v", codecs)
	}
}

func TestParseRtpmap_ExactEncodingNames(t *testing.T) {
	tests := []struct {
		value     string
		wantCodec domain.Codec
		wantOK    bool
	}{
		{"96 opus/48000/2", domain.CodecOpus, true},
		{"9 G722/8000", domain.CodecG722, true},
		{"0 PCMU/8000", domain.CodecPCMU, true},
		{"8 PCMA/8000", domain.CodecPCMA, true},
		{"97 L16/48000/1", domain.CodecL16, true},
		{"100 G7221/16000", "", false},
		{"101 telephone-event/8000", "", false},
		{"102 G729/8000", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			_, codec, ok := parseRtpmap(tt.value)
			if ok != tt.wantOK || codec != tt.wantCodec {
				t.Errorf("parseRtpmap(%q) = (%q, %v), want (%q, %v)", tt.value, codec, ok, tt.wantCodec, tt.wantOK)
			}
		})
	}
}

func TestBuildSDPOffer_DeclaresPtime(t *testing.T) {
	offer, err := BuildSDPOffer("192.168.1.10", 10002, domain.CodecG722)
	if err != nil {
		t.Fatalf("BuildSDPOffer failed: %v", err)
	}
	// The pacer emits a packet every 20ms; the offer must say so.
	if !strings.Contains(offer, "a=ptime:20") {
		t.Errorf("expected a=ptime:20 in the SDP offer, got:\n%s", offer)
	}
}
