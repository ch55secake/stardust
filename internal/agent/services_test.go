package agent

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ch55secake/stardust/internal/config"
	"github.com/ch55secake/stardust/internal/services"
)

func TestServicePolicyIsWiredIntoServer(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := config.Default()
		if enabled {
			policy, err := services.NewPolicy("0123456789abcdef0123456789abcdef", nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ServicePolicy = policy
		}
		server := NewServer(cfg, nil)
		recorder := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/v1/capabilities", nil))
		want := `"enabled":false`
		if enabled {
			want = `"enabled":true`
		}
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("policy wiring: %s", recorder.Body.String())
		}
		// No credentials: never reach systemctl even on a systemd test host.
		recorder = httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/v1/services", nil))
		wantStatus := 503
		if enabled {
			wantStatus = 401
		}
		if recorder.Code != wantStatus {
			t.Fatal(recorder.Body.String())
		}
	}
}
