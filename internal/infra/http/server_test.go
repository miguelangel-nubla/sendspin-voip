package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

type dummySIPCaller struct{}

func (d *dummySIPCaller) Start(ctx context.Context) error { return nil }
func (d *dummySIPCaller) Stop() error                     { return nil }
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
func (d *dummyIngress) SendStopToUpstream(playerID string)             {}
func (d *dummyIngress) SendNextToUpstream(playerID string)             {}
func (d *dummyIngress) SendPlayPauseToUpstream(playerID string)        {}
func (d *dummyIngress) SendVolumeToUpstream(playerID string, vol int)  {}
func (d *dummyIngress) SendMuteToUpstream(playerID string, muted bool) {}
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
			DrainDelayMs:      50,
			IdleHangupDelayMs: 2000,
		},
		arbiter,
		sipCaller,
		rtpStreamer,
		ingress,
		nil,
	)

	_ = bridge.RegisterPlayers([]domain.PlayerConfig{
		{
			ID:        "desk_phone",
			Name:      "Office Desk Phone",
			SIPTarget: "sip:101@127.0.0.1",
			Codec:     domain.CodecG722,
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

func TestHTTPServer_APITokenAuth(t *testing.T) {
	sipCaller := &dummySIPCaller{}
	bridge := app.NewBridgeService(
		nil,
		app.BridgeConfig{},
		domain.NewTargetArbiter(""),
		sipCaller,
		&dummyRTPStreamer{},
		&dummyIngress{},
		nil,
	)

	srv := NewServer(nil, ServerConfig{
		Listen:   ":8080",
		APIToken: "secret-token",
		Version:  "test",
	}, bridge, sipCaller)

	req := httptest.NewRequest(http.MethodGet, "/api/info", nil)
	rr := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rr.Code)
	}

	reqOK := httptest.NewRequest(http.MethodGet, "/api/info?token=secret-token", nil)
	rrOK := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rrOK, reqOK)
	if rrOK.Code != http.StatusOK {
		t.Fatalf("expected 200 with query token, got %d", rrOK.Code)
	}

	reqBearer := httptest.NewRequest(http.MethodGet, "/api/info", nil)
	reqBearer.Header.Set("Authorization", "Bearer secret-token")
	rrBearer := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rrBearer, reqBearer)
	if rrBearer.Code != http.StatusOK {
		t.Fatalf("expected 200 with bearer token, got %d", rrBearer.Code)
	}
}

func TestHTTPServer_PprofDisabledByDefault(t *testing.T) {
	sipCaller := &dummySIPCaller{}
	bridge := app.NewBridgeService(
		nil,
		app.BridgeConfig{},
		domain.NewTargetArbiter(""),
		sipCaller,
		&dummyRTPStreamer{},
		&dummyIngress{},
		nil,
	)
	srv := NewServer(nil, ServerConfig{Listen: ":8080"}, bridge, sipCaller)

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	rr := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for pprof when disabled, got %d", rr.Code)
	}
}

func TestHTTPServer_Metrics(t *testing.T) {
	sipCaller := &dummySIPCaller{}
	bridge := app.NewBridgeService(
		nil,
		app.BridgeConfig{},
		domain.NewTargetArbiter(""),
		sipCaller,
		&dummyRTPStreamer{},
		&dummyIngress{},
		nil,
	)
	_ = bridge.RegisterPlayers([]domain.PlayerConfig{
		{
			ID:        "kitchen_speaker",
			Name:      "Kitchen Speaker",
			SIPTarget: "sip:102@127.0.0.1",
			Codec:     domain.CodecOpus,
		},
	})

	srv := NewServer(nil, ServerConfig{
		Listen:    ":8080",
		Version:   "1.2.3",
		Commit:    "fedcba",
		BuildDate: "2026-08-22",
	}, bridge, sipCaller)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /metrics, got %d", rr.Code)
	}

	body := rr.Body.String()
	for _, expected := range []string{
		"sendspin_voip_build_info",
		"sendspin_voip_uptime_seconds",
		"sendspin_voip_sip_registered",
		"sendspin_voip_players_total 1",
		"sendspin_voip_player_active",
		"go_goroutines",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("metrics response missing %q:\n%s", expected, body)
		}
	}
}

func TestHTTPServer_HealthzAndReadyz(t *testing.T) {
	sipCaller := &dummySIPCaller{}
	bridge := app.NewBridgeService(
		nil,
		app.BridgeConfig{},
		domain.NewTargetArbiter(""),
		sipCaller,
		&dummyRTPStreamer{},
		&dummyIngress{},
		nil,
	)
	srv := NewServer(nil, ServerConfig{
		Listen:   ":8080",
		APIToken: "protected-token",
		Version:  "1.0.0",
	}, bridge, sipCaller)

	// 1. Healthz should succeed without auth
	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rrHealth := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rrHealth, reqHealth)
	if rrHealth.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /healthz, got %d", rrHealth.Code)
	}

	// 2. Readyz should succeed without auth
	reqReady := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rrReady := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rrReady, reqReady)
	if rrReady.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /readyz, got %d", rrReady.Code)
	}
}
