package sip

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	sipgotypes "github.com/emiago/sipgo/sip"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

func TestBuildAutoAnswerHeaders(t *testing.T) {
	c, err := NewCaller(nil, CallerConfig{
		LocalIP:      "127.0.0.1",
		LocalSIPPort: 5060,
	})
	if err != nil {
		t.Fatalf("NewCaller failed: %v", err)
	}

	tests := []struct {
		preset domain.AutoAnswerPreset
		custom string
		count  int
	}{
		{domain.AutoAnswerIntercom, "", 2},
		{domain.AutoAnswerYealink, "", 1},
		{domain.AutoAnswerGrandstream, "", 1},
		{domain.AutoAnswerSnom, "", 2},
		{domain.AutoAnswerCallInfo, "", 1},
		{domain.AutoAnswerPAutoAnswer, "", 1},
		{domain.AutoAnswerDefault, "", 2},
		{domain.AutoAnswerCustom, "X-Auto-Answer: 1", 1},
		{domain.AutoAnswerNone, "", 0},
	}

	for _, tt := range tests {
		hdrs := c.buildAutoAnswerHeaders(tt.preset, tt.custom)
		if len(hdrs) != tt.count {
			t.Errorf("buildAutoAnswerHeaders(%s) returned %d headers, want %d", tt.preset, len(hdrs), tt.count)
		}
	}
}

func getFreePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestCaller_MockUAS_ProbeAndDial(t *testing.T) {
	uasPort := getFreePort(t)
	uasAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(uasPort))

	uasUA, err := sipgo.NewUA(sipgo.WithUserAgent("mock-uas/1.0"), sipgo.WithUserAgentHostname("127.0.0.1"))
	if err != nil {
		t.Fatalf("failed to create mock UAS UA: %v", err)
	}
	defer uasUA.Close()

	uasServer, err := sipgo.NewServer(uasUA)
	if err != nil {
		t.Fatalf("failed to create mock UAS Server: %v", err)
	}
	defer uasServer.Close()

	// SDP Answer payload for mock phone
	sdpAnswer := "v=0\r\n" +
		"o=mockphone 12345 12345 IN IP4 127.0.0.1\r\n" +
		"s=Mock Call\r\n" +
		"c=IN IP4 127.0.0.1\r\n" +
		"t=0 0\r\n" +
		"m=audio 40000 RTP/AVP 9 0 8\r\n" +
		"a=rtpmap:9 G722/8000\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:8 PCMA/8000\r\n" +
		"a=sendrecv\r\n"

	uasServer.OnOptions(func(req *sipgotypes.Request, tx sipgotypes.ServerTransaction) {
		res := sipgotypes.NewResponseFromRequest(req, 200, "OK", []byte(sdpAnswer))
		res.AppendHeader(sipgotypes.NewHeader("Content-Type", "application/sdp"))
		_ = tx.Respond(res)
	})

	uasServer.OnInvite(func(req *sipgotypes.Request, tx sipgotypes.ServerTransaction) {
		res := sipgotypes.NewResponseFromRequest(req, 200, "OK", []byte(sdpAnswer))
		res.AppendHeader(sipgotypes.NewHeader("Content-Type", "application/sdp"))
		res.AppendHeader(&sipgotypes.ContactHeader{
			Address: sipgotypes.Uri{
				User: "100",
				Host: "127.0.0.1",
				Port: uasPort,
			},
		})
		_ = tx.Respond(res)
	})

	uasServer.OnAck(func(req *sipgotypes.Request, tx sipgotypes.ServerTransaction) {
		// ACK received
	})

	uasServer.OnBye(func(req *sipgotypes.Request, tx sipgotypes.ServerTransaction) {
		res := sipgotypes.NewResponseFromRequest(req, 200, "OK", nil)
		_ = tx.Respond(res)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = uasServer.ListenAndServe(ctx, "udp", uasAddr)
	}()

	time.Sleep(50 * time.Millisecond)

	// Create caller under test
	callerPort := getFreePort(t)
	caller, err := NewCaller(nil, CallerConfig{
		LocalIP:      "127.0.0.1",
		LocalSIPPort: callerPort,
		Mode:         "direct",
	})
	if err != nil {
		t.Fatalf("NewCaller failed: %v", err)
	}

	if err := caller.Start(ctx); err != nil {
		t.Fatalf("caller.Start failed: %v", err)
	}
	defer func() { _ = caller.Stop() }()

	time.Sleep(50 * time.Millisecond)

	// 1. Test ProbeTarget
	probeCtx, probeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer probeCancel()

	targetURI := "sip:100@" + uasAddr
	codecs, err := caller.ProbeTarget(probeCtx, targetURI)
	if err != nil {
		t.Fatalf("ProbeTarget failed: %v", err)
	}
	if len(codecs) == 0 {
		t.Fatalf("expected discovered codecs, got none")
	}
	if codecs[0] != domain.CodecG722 {
		t.Errorf("expected first codec G722, got %s", codecs[0])
	}

	// 2. Test Dial
	dialCtx, dialCancel := context.WithTimeout(ctx, 3*time.Second)
	defer dialCancel()

	player := domain.PlayerConfig{
		ID:         "test_player",
		Name:       "Test Player",
		SIPTarget:  targetURI,
		Codec:      domain.CodecG722,
		AutoAnswer: domain.AutoAnswerIntercom,
	}

	dialog, err := caller.Dial(dialCtx, player, 30000)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if dialog == nil {
		t.Fatalf("expected dialog, got nil")
	}
	if dialog.RemoteRTPAddr() == nil || dialog.RemoteRTPAddr().Port != 40000 {
		t.Errorf("expected remote RTP port 40000, got %v", dialog.RemoteRTPAddr())
	}
	if dialog.RemoteCodec() != domain.CodecG722 {
		t.Errorf("expected negotiated codec G722, got %s", dialog.RemoteCodec())
	}

	// 3. Test Bye
	byeCtx, byeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer byeCancel()
	if err := dialog.Bye(byeCtx); err != nil {
		t.Errorf("dialog.Bye failed: %v", err)
	}
}

func TestParseExpiresHeader(t *testing.T) {
	// 1. Standard Expires header
	res := sipgotypes.NewResponse(200, "OK")
	res.AppendHeader(sipgotypes.NewHeader("Expires", "120"))
	if exp := parseExpiresHeader(res); exp != 120*time.Second {
		t.Errorf("expected 120s, got %v", exp)
	}

	// 2. Contact header expires parameter
	res2 := sipgotypes.NewResponse(200, "OK")
	contact := sipgotypes.ContactHeader{
		Address: sipgotypes.Uri{User: "sendspin", Host: "127.0.0.1"},
		Params:  sipgotypes.HeaderParams{sipgotypes.HeaderKV{K: "expires", V: "300"}},
	}
	res2.AppendHeader(&contact)
	if exp := parseExpiresHeader(res2); exp != 300*time.Second {
		t.Errorf("expected 300s, got %v", exp)
	}

	// 3. Fallback default
	res3 := sipgotypes.NewResponse(200, "OK")
	if exp := parseExpiresHeader(res3); exp != 3600*time.Second {
		t.Errorf("expected default 3600s, got %v", exp)
	}
}

func TestParseInfoDTMF(t *testing.T) {
	tests := []struct {
		body     string
		expected string
	}{
		{"Signal=5\r\nDuration=160\r\n", "5"},
		{"Signal = #\nDuration=100\n", "#"},
		{"Signal=*\n", "*"},
		{"Signal=A\n", "A"},
		{"invalid body", ""},
	}

	for _, tt := range tests {
		if digit := parseInfoDTMF(tt.body); digit != tt.expected {
			t.Errorf("parseInfoDTMF(%q) = %q, want %q", tt.body, digit, tt.expected)
		}
	}
}

func TestDialogWrapper_DynamicSDPUpdate(t *testing.T) {
	d := &DialogWrapper{
		remoteRTPAddr: &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40000},
		codec:         domain.CodecG722,
		localRTPPort:  30000,
		callID:        "test-call-id",
		doneChan:      make(chan struct{}),
	}

	var updatedAddr *net.UDPAddr
	var updatedCodec domain.Codec
	d.SetSDPUpdateHandler(func(remoteAddr *net.UDPAddr, codec domain.Codec) {
		updatedAddr = remoteAddr
		updatedCodec = codec
	})

	newAddr := &net.UDPAddr{IP: net.ParseIP("10.0.0.5"), Port: 45000}
	d.updateRemoteSDP(newAddr, domain.CodecOpus)

	if d.RemoteRTPAddr().String() != "10.0.0.5:45000" {
		t.Errorf("expected updated address 10.0.0.5:45000, got %v", d.RemoteRTPAddr())
	}
	if d.RemoteCodec() != domain.CodecOpus {
		t.Errorf("expected updated codec Opus, got %s", d.RemoteCodec())
	}
	if updatedAddr.String() != "10.0.0.5:45000" || updatedCodec != domain.CodecOpus {
		t.Errorf("callback not fired with expected values: addr=%v codec=%v", updatedAddr, updatedCodec)
	}
}
