package clashapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/trafficcontrol"
	C "github.com/sagernet/sing-box/constant"
	M "github.com/sagernet/sing/common/metadata"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
)

func TestTrafficMapStateAndRealIPCapture(t *testing.T) {
	tm := trafficcontrol.NewManager()
	srv := &Server{
		ctx:            t.Context(),
		trafficManager: tm,
	}

	router := chi.NewRouter()
	srv.setupTrafficMap(router, "test-secret")

	// 1. Verify /map HTML UI loads without error
	reqUI := httptest.NewRequest(http.MethodGet, "/map", nil)
	recUI := httptest.NewRecorder()
	router.ServeHTTP(recUI, reqUI)
	if recUI.Code != http.StatusOK {
		t.Fatalf("expected /map status 200, got %d", recUI.Code)
	}
	if !strings.Contains(recUI.Body.String(), "sing-box 全球流量轨迹雷达") {
		t.Fatalf("expected /map HTML body to contain title")
	}

	// 2. Simulate a domain connection (google.com) where initial Destination.Addr is empty,
	//    and then socket handshake records the final server IP (142.250.190.46)
	id, _ := uuid.NewV4()
	meta := &trafficcontrol.TrackerMetadata{
		ID: id,
		Metadata: adapter.InboundContext{
			Inbound:     "vless-reality-in",
			InboundType: C.TypeVLESS,
			Network:     "tcp",
			Source:      M.ParseSocksaddr("203.0.113.88:54321"),
			Destination: M.ParseSocksaddr("www.google.com:443"),
			Domain:      "www.google.com",
		},
		CreatedAt:           time.Now(),
		Upload:              new(atomic.Int64),
		Download:            new(atomic.Int64),
		Chain:               []string{"direct"},
		Outbound:            "direct",
		OutboundType:        C.TypeDirect,
		ResolvedDestination: new(atomic.Pointer[netip.Addr]),
		OutboundDestination: new(atomic.Pointer[netip.Addr]),
	}
	meta.Upload.Store(1024)
	meta.Download.Store(8192)

	// Before handshake: DestinationAddr should be invalid
	if meta.DestinationAddr().IsValid() {
		t.Fatalf("expected empty DestinationAddr before handshake, got %v", meta.DestinationAddr())
	}

	// Simulate TCP handshake completion with Google's final server IP
	finalGoogleIP := netip.MustParseAddr("142.250.190.46")
	meta.RecordRemoteAddr(finalGoogleIP)

	// After handshake: DestinationAddr must equal 142.250.190.46
	if got := meta.DestinationAddr(); got != finalGoogleIP {
		t.Fatalf("expected final DestinationAddr %v, got %v", finalGoogleIP, got)
	}

	// 3. Verify /map/api/state rejects unauthorized requests and succeeds with Bearer token
	reqUnauth := httptest.NewRequest(http.MethodGet, "/map/api/state", nil)
	recUnauth := httptest.NewRecorder()
	router.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", recUnauth.Code)
	}

	reqState := httptest.NewRequest(http.MethodGet, "/map/api/state", nil)
	reqState.Header.Set("Authorization", "Bearer test-secret")
	recState := httptest.NewRecorder()
	router.ServeHTTP(recState, reqState)
	if recState.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d", recState.Code)
	}
}
