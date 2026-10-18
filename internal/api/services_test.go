package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ch55secake/stardust/internal/services"
)

const serviceToken = "0123456789abcdef0123456789abcdef"
const restartToken = "fedcba9876543210fedcba9876543210"

type fakeInspector struct {
	supported bool
	names     []string
	inspect   func(context.Context, string) (services.Unit, error)
}

type fakeRestarter struct {
	supported bool
	calls     atomic.Int32
	restart   func(context.Context, string) error
}

func (f *fakeRestarter) Supported() bool { return f.supported }
func (f *fakeRestarter) Restart(ctx context.Context, name string) error {
	f.calls.Add(1)
	if f.restart != nil {
		return f.restart(ctx, name)
	}
	return nil
}

func (f *fakeInspector) Supported() bool { return f.supported }
func (f *fakeInspector) Inspect(ctx context.Context, name string) (services.Unit, error) {
	f.names = append(f.names, name)
	if f.inspect != nil {
		return f.inspect(ctx, name)
	}
	return services.Unit{Name: name, ID: name, LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 42, MainProcess: &services.ProcessIdentity{Name: "worker", EffectiveUID: 1001}}, nil
}

func serviceHandler(t *testing.T, inspector *fakeInspector, names ...string) http.Handler {
	t.Helper()
	policy, err := services.NewPolicy(serviceToken, names)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(fixtureCollector{}, 10*time.Millisecond, testLogger(), WithServices(policy, inspector))
}

func serviceRequest(handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func restartServiceHandler(t *testing.T, inspector *fakeInspector, restarter *fakeRestarter) (http.Handler, *services.RestartManager) {
	t.Helper()
	policy, err := services.NewPolicyWithRestart(serviceToken, []string{"a.service", "b.service"}, restartToken, []string{"a.service"})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	manager, err := services.NewRestartManager(filepath.Join(directory, "operations.json"), policy, restarter, inspector, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return NewHandler(fixtureCollector{}, time.Second, testLogger(), WithServices(policy, inspector, manager)), manager
}

func restartRequest(handler http.Handler, method, path, token, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestServiceAuthorizationAndMethods(t *testing.T) {
	inspector := &fakeInspector{supported: true}
	handler := serviceHandler(t, inspector, "a.service")
	for _, test := range []struct {
		method, path, token, code string
		status                    int
	}{
		{"GET", "/api/v1/services", "", "unauthorized", 401},
		{"GET", "/api/v1/services/a.service", "wrong", "unauthorized", 401},
		{"GET", "/api/v1/services/hidden.service", serviceToken, "unit_forbidden", 403},
		{"GET", "/api/v1/services/bad;id.service", serviceToken, "invalid_unit", 400},
		{"POST", "/api/v1/services/a.service", serviceToken, "method_not_allowed", 405},
		{"DELETE", "/api/v1/services", serviceToken, "method_not_allowed", 405},
	} {
		recorder := serviceRequest(handler, test.method, test.path, test.token)
		if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.code) {
			t.Fatalf("%s: %d %s", test.path, recorder.Code, recorder.Body.String())
		}
		if test.status == 401 && recorder.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("missing auth challenge")
		}
		if test.status == 405 && recorder.Header().Get("Allow") != "GET" {
			t.Fatal("missing Allow")
		}
		if strings.Contains(recorder.Body.String(), serviceToken) {
			t.Fatal("token leaked")
		}
	}
	request := httptest.NewRequest("GET", "/api/v1/services", nil)
	request.Header.Add("Authorization", "Bearer "+serviceToken)
	request.Header.Add("Authorization", "Bearer "+serviceToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != 401 {
		t.Fatal("duplicate auth accepted")
	}
	if len(inspector.names) != 0 {
		t.Fatalf("unauthorized request reached backend: %v", inspector.names)
	}
}

func TestServiceBearerSchemeIsCaseInsensitive(t *testing.T) {
	handler := serviceHandler(t, &fakeInspector{supported: true}, "a.service")
	request := httptest.NewRequest(http.MethodGet, "/api/v1/services", nil)
	request.Header.Set("Authorization", "bEaReR "+serviceToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("mixed-case bearer scheme rejected: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestServiceCapabilitiesDisabledUnsupportedAndEmpty(t *testing.T) {
	disabled := NewHandler(fixtureCollector{}, time.Second, testLogger())
	for _, handler := range []http.Handler{disabled, serviceHandler(t, &fakeInspector{}), serviceHandler(t, &fakeInspector{supported: true})} {
		recorder := serviceRequest(handler, "GET", "/api/v1/capabilities", "")
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"restart":false`) || !strings.Contains(recorder.Body.String(), `"logs":false`) {
			t.Fatal(recorder.Body.String())
		}
		var response struct {
			Timestamp time.Time `json:"timestamp"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Timestamp.IsZero() {
			t.Fatalf("capability timestamp missing or invalid: %s (%v)", recorder.Body.String(), err)
		}
	}
	if recorder := serviceRequest(disabled, "GET", "/api/v1/services", serviceToken); recorder.Code != 503 || !strings.Contains(recorder.Body.String(), "services_disabled") {
		t.Fatal(recorder.Body.String())
	}
	unsupported := serviceHandler(t, &fakeInspector{})
	if recorder := serviceRequest(unsupported, "GET", "/api/v1/services", serviceToken); recorder.Code != 501 {
		t.Fatal(recorder.Body.String())
	}
	empty := serviceHandler(t, &fakeInspector{supported: true})
	if recorder := serviceRequest(empty, "GET", "/api/v1/services", serviceToken); recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"units":[]`) {
		t.Fatal(recorder.Body.String())
	}
	// Existing clients remain compatible: service credentials do not change
	// the historical health/metrics authentication policy.
	for _, path := range []string{"/api/v1/health", "/api/v1/metrics"} {
		if recorder := serviceRequest(empty, "GET", path, ""); recorder.Code != 200 {
			t.Fatal(recorder.Body.String())
		}
	}
}

func TestRestartEndpointFailsClosedBeforeMutation(t *testing.T) {
	inspector := &fakeInspector{supported: true}
	restarter := &fakeRestarter{supported: true}
	handler, _ := restartServiceHandler(t, inspector, restarter)
	key := strings.Repeat("i", 32)
	for _, test := range []struct {
		name, method, path, token string
		wantCode                  int
		wantAllow                 string
	}{
		{"GET does not mutate", http.MethodGet, "/api/v1/services/a.service/restart", restartToken, http.StatusMethodNotAllowed, http.MethodPost},
		{"missing restart token", http.MethodPost, "/api/v1/services/a.service/restart", "", http.StatusUnauthorized, ""},
		{"read token is insufficient", http.MethodPost, "/api/v1/services/a.service/restart", serviceToken, http.StatusUnauthorized, ""},
		{"unit outside restart allowlist", http.MethodPost, "/api/v1/services/b.service/restart", restartToken, http.StatusForbidden, ""},
		{"malformed unit", http.MethodPost, "/api/v1/services/bad;id.service/restart", restartToken, http.StatusBadRequest, ""},
		{"query parameters rejected", http.MethodPost, "/api/v1/services/a.service/restart?unexpected=true", restartToken, http.StatusBadRequest, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := restartRequest(handler, test.method, test.path, test.token, key)
			if recorder.Code != test.wantCode {
				t.Fatalf("got %d want %d: %s", recorder.Code, test.wantCode, recorder.Body.String())
			}
			if test.wantAllow != "" && recorder.Header().Get("Allow") != test.wantAllow {
				t.Fatalf("Allow = %q", recorder.Header().Get("Allow"))
			}
			if recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("restart response may be cached")
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/services/a.service/restart", nil)
	request.Header.Set("Authorization", "Bearer "+restartToken)
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Origin", "https://attacker.example")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "origin_forbidden") {
		t.Fatalf("Origin request was not rejected: %d %s", recorder.Code, recorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/services/a.service/restart", strings.NewReader("unexpected body"))
	request.Header.Set("Authorization", "Bearer "+restartToken)
	request.Header.Set("Idempotency-Key", key)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("request body accepted: %d %s", recorder.Code, recorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/services/a.service/restart", nil)
	request.Header.Set("Authorization", "Bearer "+restartToken)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_idempotency_key") {
		t.Fatalf("missing idempotency key accepted: %d %s", recorder.Code, recorder.Body.String())
	}
	if restarter.calls.Load() != 0 || len(inspector.names) != 0 {
		t.Fatalf("rejected request reached backend: restarts=%d inspections=%v", restarter.calls.Load(), inspector.names)
	}
}

func TestRestartEndpointIdempotencyStatusAndSeparateAuthorization(t *testing.T) {
	inspector := &fakeInspector{supported: true}
	restarter := &fakeRestarter{supported: true}
	handler, manager := restartServiceHandler(t, inspector, restarter)
	key := strings.Repeat("r", 40)
	recorder := restartRequest(handler, http.MethodPost, "/api/v1/services/a.service/restart", restartToken, key)
	if recorder.Code != http.StatusAccepted || !strings.Contains(recorder.Body.String(), `"state":"running"`) {
		t.Fatalf("restart was not accepted asynchronously: %d %s", recorder.Code, recorder.Body.String())
	}
	var accepted struct {
		Data services.RestartOperation `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil || accepted.Data.ID == "" {
		t.Fatalf("operation ID missing: %s (%v)", recorder.Body.String(), err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		operation, err := manager.Status(accepted.Data.ID)
		if err != nil {
			t.Fatal(err)
		}
		if operation.State != services.RestartRunning {
			break
		}
		time.Sleep(time.Millisecond)
	}
	statusPath := "/api/v1/service-operations/" + accepted.Data.ID
	if recorder := restartRequest(handler, http.MethodGet, statusPath, serviceToken, ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("read token authorized status polling: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := restartRequest(handler, http.MethodGet, statusPath, restartToken, ""); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"state":"succeeded"`) {
		t.Fatalf("restart status unavailable: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := restartRequest(handler, http.MethodPost, statusPath, restartToken, ""); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status endpoint accepted a mutating method: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = restartRequest(handler, http.MethodPost, "/api/v1/services/a.service/restart", restartToken, key)
	if recorder.Code != http.StatusOK || restarter.calls.Load() != 1 {
		t.Fatalf("retry did not return prior operation: %d calls=%d %s", recorder.Code, restarter.calls.Load(), recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), restartToken) || strings.Contains(recorder.Body.String(), key) {
		t.Fatal("credential or idempotency key leaked in response")
	}
}

func TestRestartEndpointUnsupportedHostFailsClosed(t *testing.T) {
	inspector := &fakeInspector{}
	restarter := &fakeRestarter{supported: true}
	handler, _ := restartServiceHandler(t, inspector, restarter)
	recorder := restartRequest(handler, http.MethodPost, "/api/v1/services/a.service/restart", restartToken, strings.Repeat("u", 32))
	if recorder.Code != http.StatusNotImplemented || !strings.Contains(recorder.Body.String(), "services_unsupported") || restarter.calls.Load() != 0 {
		t.Fatalf("unsupported host did not fail closed: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestServicePaginationFilteringAndLiveDetails(t *testing.T) {
	inspector := &fakeInspector{supported: true}
	handler := serviceHandler(t, inspector, "z.service", "worker@blue.service", "a.service")
	recorder := serviceRequest(handler, "GET", "/api/v1/services?limit=1", serviceToken)
	var response struct {
		APIVersion string      `json:"api_version"`
		Timestamp  time.Time   `json:"timestamp"`
		Data       ServicePage `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || response.APIVersion != Version || response.Timestamp.IsZero() || len(response.Data.Units) != 1 || response.Data.NextCursor != "a.service" {
		t.Fatal(recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("service data may be cached")
	}
	assertServiceFixture(t, recorder, "services-v1.json")
	recorder = serviceRequest(handler, "GET", "/api/v1/services?limit=1&cursor=a.service&filter=worker", serviceToken)
	response.Data = ServicePage{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || response.Data.Units[0].Name != "worker@blue.service" || response.Data.NextCursor != "" {
		t.Fatal(recorder.Body.String())
	}
	if !reflect.DeepEqual(inspector.names, []string{"a.service", "worker@blue.service"}) {
		t.Fatalf("inspected units outside requested page: %v", inspector.names)
	}
	pid := uint32(10)
	inspector.inspect = func(_ context.Context, name string) (services.Unit, error) {
		pid++
		return services.Unit{Name: name, ID: "canonical.service", MainPID: pid, MainProcess: &services.ProcessIdentity{Name: "worker", EffectiveUID: pid}, ActiveState: "failed", SubState: "failed"}, nil
	}
	for index, want := range []string{`"main_pid":11`, `"main_pid":12`} {
		recorder = serviceRequest(handler, "GET", "/api/v1/services/a.service", serviceToken)
		wantUID := uint32(11 + index)
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), want) || !strings.Contains(recorder.Body.String(), `"id":"canonical.service"`) || !strings.Contains(recorder.Body.String(), fmt.Sprintf(`"effective_uid":%d`, wantUID)) {
			t.Fatal(recorder.Body.String())
		}
		var detail struct {
			Timestamp time.Time `json:"timestamp"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil || detail.Timestamp.IsZero() {
			t.Fatalf("detail timestamp missing or invalid: %s (%v)", recorder.Body.String(), err)
		}
	}
}

func TestServiceInvalidQueries(t *testing.T) {
	inspector := &fakeInspector{supported: true}
	handler := serviceHandler(t, inspector, "a.service")
	for _, query := range []string{"limit=0", "limit=51", "limit=x", "limit=1&limit=2", "cursor=unknown.service", "cursor=../x", "filter=" + strings.Repeat("a", 256), "surprise=true", "filter=%zz", "filter=a;b"} {
		if recorder := serviceRequest(handler, "GET", "/api/v1/services?"+query, serviceToken); recorder.Code != 400 {
			t.Fatalf("query %q: %d %s", query, recorder.Code, recorder.Body.String())
		}
	}
	if len(inspector.names) != 0 {
		t.Fatal("invalid query reached backend")
	}
}

func TestServiceErrorsAndDeadlines(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{services.ErrNotFound, 404, "unit_not_found"},
		{services.ErrForbidden, 403, "unit_forbidden"},
		{services.ErrPermissionDenied, 503, "services_permission_denied"},
		{services.ErrUnsupported, 501, "services_unsupported"},
		{errors.New("permission denied: sensitive details"), 503, "services_unavailable"},
		{context.DeadlineExceeded, 504, "services_timeout"},
		{context.Canceled, 504, "services_timeout"},
	} {
		inspector := &fakeInspector{supported: true, inspect: func(context.Context, string) (services.Unit, error) { return services.Unit{}, test.err }}
		handler := serviceHandler(t, inspector, "a.service")
		recorder := serviceRequest(handler, "GET", "/api/v1/services/a.service", serviceToken)
		if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.code) || strings.Contains(recorder.Body.String(), "sensitive") {
			t.Fatal(recorder.Body.String())
		}
		if errors.Is(test.err, services.ErrNotFound) {
			recorder = serviceRequest(handler, "GET", "/api/v1/services", serviceToken)
			if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"load_state":"not-found"`) {
				t.Fatal(recorder.Body.String())
			}
		}
	}
	inspector := &fakeInspector{supported: true, inspect: func(ctx context.Context, _ string) (services.Unit, error) {
		<-ctx.Done()
		return services.Unit{}, ctx.Err()
	}}
	if recorder := serviceRequest(serviceHandler(t, inspector, "a.service"), "GET", "/api/v1/services", serviceToken); recorder.Code != 504 {
		t.Fatal(recorder.Body.String())
	}
}

func assertServiceFixture(t *testing.T, recorder *httptest.ResponseRecorder, name string) {
	t.Helper()
	fixture, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	gotObject, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("fixture %s response is not an object", name)
	}
	timestamp, ok := gotObject["timestamp"].(string)
	if !ok {
		t.Fatalf("fixture %s response omitted timestamp", name)
	}
	if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
		t.Fatalf("fixture %s has invalid timestamp %q: %v", name, timestamp, err)
	}
	delete(gotObject, "timestamp")
	if recorder.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture %s mismatch: %d %s", name, recorder.Code, recorder.Body.String())
	}
}

func TestServiceStatesAndUnsupportedFixtures(t *testing.T) {
	fixture, err := os.ReadFile("testdata/services-states-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Data ServicePage `json:"data"`
	}
	if err := json.Unmarshal(fixture, &response); err != nil {
		t.Fatal(err)
	}
	units := make(map[string]services.Unit)
	var names []string
	for _, unit := range response.Data.Units {
		units[unit.Name] = unit
		names = append(names, unit.Name)
	}
	inspector := &fakeInspector{supported: true, inspect: func(_ context.Context, name string) (services.Unit, error) {
		if name == "missing.service" {
			return services.Unit{}, services.ErrNotFound
		}
		return units[name], nil
	}}
	assertServiceFixture(t, serviceRequest(serviceHandler(t, inspector, names...), "GET", "/api/v1/services", serviceToken), "services-states-v1.json")
	assertServiceFixture(t, serviceRequest(serviceHandler(t, &fakeInspector{}), "GET", "/api/v1/capabilities", ""), "services-unsupported-v1.json")
}

func TestServiceDescriptionsRemainJSONData(t *testing.T) {
	description := "example\n\x1b[31m$(touch /not-executed) café"
	inspector := &fakeInspector{supported: true, inspect: func(_ context.Context, name string) (services.Unit, error) {
		return services.Unit{Name: name, Description: description}, nil
	}}
	recorder := serviceRequest(serviceHandler(t, inspector, "a.service"), "GET", "/api/v1/services/a.service", serviceToken)
	var response struct {
		Data services.Unit `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || response.Data.Description != description || strings.ContainsRune(recorder.Body.String(), '\x1b') {
		t.Fatal("description was evaluated or emitted as raw terminal controls")
	}
}
