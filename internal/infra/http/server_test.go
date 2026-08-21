package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

type dummySIPCaller struct{}

func (d *dummySIPCaller) Start(ctx context.Context) error { return nil }
func (d *dummySIPCaller) Stop() error                    { return nil }
func (d *dummySIPCaller) LocalIP() string                 { return "127.0.0.1" }
func (d *dummySIPCaller) RegistrationStatus() app.SIPStatus {
	return app.SIPStatus{
		Mode:       "pbx",
		Server:     "sip.example.com:5060",
		Registered: true,
	}
}
func (d *dummySIPCaller) ProbeTarget(ctx context.Context, targetURI string) ([]domain.Codec, error) {
	return []domain.Codec{domain.CodecOpus, domain.CodecL16, domain.CodecG722, domain.CodecPCMU, domain.CodecPCMA}, nil
}
func (d *dummySIPCaller) Dial(ctx context.Context, player domain.PlayerConfig, localRTPPort int) (app.SIPDialog, error) {
	return nil, nil
}

type dummyRTPStreamer struct{}

func (d *dummyRTPStreamer) CreateSession(codec domain.Codec) (app.RTPSession, error) {
	return nil, nil
}

type dummyIngress struct{}

func (d *dummyIngress) RegisterPlayer(player domain.PlayerConfig, handler app.PlayerEventHandler) error {
	return nil
}
func (d *dummyIngress) RegisterPlayerWithCodecs(player domain.PlayerConfig, codecs []domain.Codec, handler app.PlayerEventHandler) error {
	return nil
}
func (d *dummyIngress) UnregisterPlayer(playerID string) error {
	return nil
}
func (d *dummyIngress) SendPauseToUpstream(playerID string) {}
func (d *dummyIngress) GetPlayerStats(playerID string) (app.IngressPlayerStats, bool) {
	return app.IngressPlayerStats{
		ServerAddr:     "127.0.0.1:8927",
		Connected:      true,
		Codec:          "pcm",
		SampleRate:     16000,
		Channels:       1,
		BitDepth:       16,
		ChunksReceived: 10,
		BytesReceived:  3200,
	}, true
}
func (d *dummyIngress) StopAll() error { return nil }

func TestHTTPServer_Endpoints(t *testing.T) {
	sipCaller := &dummySIPCaller{}
	rtpStreamer := &dummyRTPStreamer{}
	ingress := &dummyIngress{}
	arbiter := domain.NewTargetArbiter(domain.ConflictPolicyPreemptAnnouncements)

	bridge := app.NewBridgeService(
		nil,
		app.BridgeConfig{
			DefaultBufferMode: domain.BufferModeAnnouncement,
			PickupBufferMs:    500,
			DrainDelayMs:      50,
			IdleHangupDelayMs: 2000,
		},
		arbiter,
		sipCaller,
		rtpStreamer,
		ingress,
	)

	_ = bridge.RegisterPlayers([]domain.PlayerConfig{
		{
			ID:         "desk_phone",
			Name:       "Office Desk Phone",
			SIPTarget:  "sip:101@127.0.0.1",
			Codec:      domain.CodecG722,
			BufferMode: domain.BufferModeLive,
		},
	})

	srv := NewServer(nil, ServerConfig{
		Listen:    ":8080",
		Version:   "1.0.0-test",
		Commit:    "abc1234",
		BuildDate: "2026-08-21",
	}, bridge, sipCaller)

	// 1. Test Dashboard HTML
	reqDashboard := httptest.NewRequest(http.MethodGet, "/", nil)
	rrDashboard := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rrDashboard, reqDashboard)

	if rrDashboard.Code != http.StatusOK {
		t.Errorf("expected 200 OK for /, got %d", rrDashboard.Code)
	}
	if ct := rrDashboard.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("unexpected content-type %s", ct)
	}

	// 2. Test /api/streams
	reqStreams := httptest.NewRequest(http.MethodGet, "/api/streams", nil)
	rrStreams := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rrStreams, reqStreams)

	if rrStreams.Code != http.StatusOK {
		t.Errorf("expected 200 OK for /api/streams, got %d", rrStreams.Code)
	}
	var streamsMap map[string]app.StreamDebugInfo
	if err := json.Unmarshal(rrStreams.Body.Bytes(), &streamsMap); err != nil {
		t.Fatalf("failed to decode /api/streams response: %v", err)
	}
	if len(streamsMap) != 1 {
		t.Errorf("expected 1 stream in map, got %d", len(streamsMap))
	}
	if st, ok := streamsMap["desk_phone"]; !ok || st.Name != "Office Desk Phone" {
		t.Errorf("unexpected stream info: %+v", streamsMap)
	}

	// 3. Test /api/streams?src=desk_phone
	reqSingleStream := httptest.NewRequest(http.MethodGet, "/api/streams?src=desk_phone", nil)
	rrSingleStream := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rrSingleStream, reqSingleStream)

	if rrSingleStream.Code != http.StatusOK {
		t.Errorf("expected 200 OK for /api/streams?src=desk_phone, got %d", rrSingleStream.Code)
	}

	// 4. Test /api/info
	reqInfo := httptest.NewRequest(http.MethodGet, "/api/info", nil)
	rrInfo := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rrInfo, reqInfo)

	if rrInfo.Code != http.StatusOK {
		t.Errorf("expected 200 OK for /api/info, got %d", rrInfo.Code)
	}
	var sysInfo SystemInfo
	if err := json.Unmarshal(rrInfo.Body.Bytes(), &sysInfo); err != nil {
		t.Fatalf("failed to decode /api/info response: %v", err)
	}
	if sysInfo.Version != "1.0.0-test" {
		t.Errorf("expected version 1.0.0-test, got %s", sysInfo.Version)
	}
	if !sysInfo.SIP.Registered {
		t.Errorf("expected SIP registered to be true")
	}

	// 5. Test /api/codecs
	reqCodecs := httptest.NewRequest(http.MethodGet, "/api/codecs", nil)
	rrCodecs := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rrCodecs, reqCodecs)

	if rrCodecs.Code != http.StatusOK {
		t.Errorf("expected 200 OK for /api/codecs, got %d", rrCodecs.Code)
	}
}
