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
}
