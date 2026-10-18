package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ch55secake/stardust/internal/config"
)

func TestServerAPITokenIsOptionalAndWiredToHandler(t *testing.T) {
	cfg := config.Default()
	legacy := NewServer(cfg, nil).httpServer.Handler
	for _, path := range []string{"/api/v1/health", "/api/v1/metrics"} {
		recorder := httptest.NewRecorder()
		legacy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("default server rejected unauthenticated %s request: %d", path, recorder.Code)
		}
	}

	const token = "0123456789abcdef0123456789abcdef"
	protected := NewServerWithAPIToken(cfg, nil, token).httpServer.Handler
	for _, path := range []string{"/api/v1/health", "/api/v1/metrics"} {
		recorder := httptest.NewRecorder()
		protected.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("protected server allowed unauthenticated %s request: %d", path, recorder.Code)
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder = httptest.NewRecorder()
		protected.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("protected server rejected valid credential for %s: %d", path, recorder.Code)
		}
	}
}
