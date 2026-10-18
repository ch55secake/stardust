package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ch55secake/stardust/internal/services"
)

const logToken = "abcdef0123456789abcdef0123456789"

type fakeJournalReader struct {
	supported bool
	unit      string
	query     services.LogQuery
	calls     int
	page      services.LogPage
	err       error
}

func (f *fakeJournalReader) Supported() bool { return f.supported }
func (f *fakeJournalReader) Read(_ context.Context, unit string, query services.LogQuery) (services.LogPage, error) {
	f.calls++
	f.unit = unit
	f.query = query
	return f.page, f.err
}

func logPolicyHandler(t *testing.T, reader *fakeJournalReader, serviceUnits, logUnits []string) http.Handler {
	t.Helper()
	policy, err := services.NewPolicyWithLogs(serviceToken, serviceUnits, logToken, logUnits)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(fixtureCollector{}, time.Second, testLogger(), WithServices(policy, &fakeInspector{supported: true}), WithServiceLogs(policy.LogReadPolicy(), reader))
}

func logRequest(handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestServiceLogsAuthorizationIsIndependentAndAllowlisted(t *testing.T) {
	reader := &fakeJournalReader{supported: true, page: services.LogPage{Entries: []services.LogEntry{}}}
	handler := logPolicyHandler(t, reader, []string{"web.service", "other.service"}, []string{"web.service"})
	for _, test := range []struct {
		token, path string
		status      int
		code        string
	}{
		{"", "/api/v1/services/web.service/logs", 401, "logs_unauthorized"},
		{serviceToken, "/api/v1/services/web.service/logs", 401, "logs_unauthorized"},
		{logToken, "/api/v1/services/other.service/logs", 403, "log_unit_forbidden"},
		{logToken, "/api/v1/services/bad;unit.service/logs", 400, "invalid_unit"},
	} {
		recorder := logRequest(handler, "GET", test.path, test.token)
		if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.code) {
			t.Fatalf("%s: %d %s", test.path, recorder.Code, recorder.Body.String())
		}
		if test.status == 401 && recorder.Header().Get("WWW-Authenticate") != `Bearer realm="service-logs"` {
			t.Fatal("wrong log auth challenge")
		}
	}
	if reader.calls != 0 {
		t.Fatal("unauthorized or unallowlisted log request reached journal")
	}
	if recorder := logRequest(handler, "GET", "/api/v1/services/web.service/logs", logToken); recorder.Code != 200 {
		t.Fatal(recorder.Body.String())
	}
	if reader.calls != 1 || reader.unit != "web.service" {
		t.Fatal("authorized log request not routed")
	}
	if recorder := logRequest(handler, "GET", "/api/v1/capabilities", ""); recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"logs":true`) {
		t.Fatal(recorder.Body.String())
	}
	if recorder := logRequest(handler, "GET", "/api/v1/services", logToken); recorder.Code != 401 {
		t.Fatal("log token unexpectedly grants service inventory")
	}
}

func TestServiceLogsQueryBoundsCursorsAndFilters(t *testing.T) {
	reader := &fakeJournalReader{supported: true, page: services.LogPage{Entries: []services.LogEntry{}}}
	handler := logPolicyHandler(t, reader, []string{"web.service"}, []string{"web.service"})
	since := "2026-10-18T10:00:00-01:00"
	until := "2026-10-18T12:00:00Z"
	path := "/api/v1/services/web.service/logs?limit=17&before=opaqueABC&since=" + urlQueryEscape(since) + "&until=" + urlQueryEscape(until)
	if recorder := logRequest(handler, "GET", path, logToken); recorder.Code != 200 {
		t.Fatal(recorder.Body.String())
	}
	if reader.query.Limit != 17 || reader.query.Before != "opaqueABC" || reader.query.Since.UTC().Format(time.RFC3339) != "2026-10-18T11:00:00Z" || reader.query.Until.UTC().Format(time.RFC3339) != "2026-10-18T12:00:00Z" {
		t.Fatalf("query not parsed correctly: %+v", reader.query)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "before=x&after=y", "since=tomorrow", "since=2026-10-19T00:00:00Z&until=2026-10-18T00:00:00Z", "cursor=x", "after=%zz"} {
		if recorder := logRequest(handler, "GET", "/api/v1/services/web.service/logs?"+query, logToken); recorder.Code != 400 || !strings.Contains(recorder.Body.String(), "invalid_log_query") {
			t.Fatalf("query %q: %d %s", query, recorder.Code, recorder.Body.String())
		}
	}
	reader.err = services.ErrLogCursorExpired
	if recorder := logRequest(handler, "GET", "/api/v1/services/web.service/logs?before=opaqueABC", logToken); recorder.Code != 410 || !strings.Contains(recorder.Body.String(), "log_cursor_expired") {
		t.Fatal(recorder.Body.String())
	}
}

func TestServiceLogsPageFixtureIsStructuredData(t *testing.T) {
	entry := services.LogEntry{Timestamp: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), Message: "started\nworker café ✓\x1b[31m"}
	priority := 6
	entry.Priority = &priority
	entry.Cursor = "opaque-cursor-1"
	older := "opaque-cursor-older"
	reader := &fakeJournalReader{supported: true, page: services.LogPage{Entries: []services.LogEntry{entry}, NextCursor: older, LatestCursor: entry.Cursor, HasMore: true}}
	handler := logPolicyHandler(t, reader, []string{"web.service"}, []string{"web.service"})
	recorder := logRequest(handler, "GET", "/api/v1/services/web.service/logs", logToken)
	if recorder.Code != 200 || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(recorder.Body.String())
	}
	fixture, err := os.ReadFile("testdata/service-logs-page-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	gotData := got["data"].(map[string]any)
	wantData := want["data"].(map[string]any)
	if !reflect.DeepEqual(gotData, wantData) {
		t.Fatalf("log response fixture mismatch: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `\u001b[31m`) || strings.ContainsRune(recorder.Body.String(), '\x1b') {
		t.Fatal("log control byte was not JSON-escaped data")
	}
	reader.page = services.LogPage{Entries: []services.LogEntry{}}
	recorder = logRequest(handler, "GET", "/api/v1/services/web.service/logs", logToken)
	emptyFixture, err := os.ReadFile("testdata/service-logs-empty-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var emptyGot, emptyWant map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &emptyGot)
	_ = json.Unmarshal(emptyFixture, &emptyWant)
	delete(emptyGot, "timestamp")
	if !reflect.DeepEqual(emptyGot, emptyWant) {
		t.Fatalf("empty log fixture mismatch: %s", recorder.Body.String())
	}
}

func TestServiceLogsErrorsAreSafeAndReadOnly(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{services.ErrLogsPermissionDenied, 503, "logs_permission_denied"},
		{services.ErrLogsUnsupported, 501, "logs_unsupported"},
		{services.ErrLogsUnavailable, 503, "logs_unavailable"},
		{context.DeadlineExceeded, 504, "logs_timeout"},
		{errors.New("sensitive journal path"), 503, "logs_unavailable"},
	} {
		reader := &fakeJournalReader{supported: true, err: test.err}
		handler := logPolicyHandler(t, reader, []string{"web.service"}, []string{"web.service"})
		recorder := logRequest(handler, "GET", "/api/v1/services/web.service/logs", logToken)
		if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.code) || strings.Contains(recorder.Body.String(), "sensitive") {
			t.Fatal(recorder.Body.String())
		}
	}
	reader := &fakeJournalReader{supported: true}
	handler := logPolicyHandler(t, reader, []string{"web.service"}, []string{"web.service"})
	if recorder := logRequest(handler, "POST", "/api/v1/services/web.service/logs", logToken); recorder.Code != 405 || recorder.Header().Get("Allow") != "GET" {
		t.Fatal(recorder.Body.String())
	}
	if reader.calls != 0 {
		t.Fatal("non-GET request reached journal reader")
	}
	unsupported := &fakeJournalReader{supported: false}
	handler = logPolicyHandler(t, unsupported, []string{"web.service"}, []string{"web.service"})
	if recorder := logRequest(handler, "GET", "/api/v1/services/web.service/logs", logToken); recorder.Code != 501 {
		t.Fatal(recorder.Body.String())
	}
}

func TestServiceLogsDistinguishMissingUnitBeforeJournalRead(t *testing.T) {
	policy, err := services.NewPolicyWithLogs(serviceToken, nil, logToken, []string{"vanished.service"})
	if err != nil {
		t.Fatal(err)
	}
	inspector := &fakeInspector{supported: true, inspect: func(context.Context, string) (services.Unit, error) { return services.Unit{}, services.ErrNotFound }}
	reader := &fakeJournalReader{supported: true}
	handler := NewHandler(fixtureCollector{}, time.Second, testLogger(), WithServices(policy, inspector), WithServiceLogs(policy.LogReadPolicy(), reader))
	recorder := logRequest(handler, "GET", "/api/v1/services/vanished.service/logs", logToken)
	if recorder.Code != 404 || !strings.Contains(recorder.Body.String(), "log_unit_not_found") {
		t.Fatal(recorder.Body.String())
	}
	if reader.calls != 0 {
		t.Fatal("journal was queried for a missing service")
	}
}

func TestServiceLogsUseCanonicalSystemdIdentity(t *testing.T) {
	policy, err := services.NewPolicyWithLogs(serviceToken, nil, logToken, []string{"web-alias.service"})
	if err != nil {
		t.Fatal(err)
	}
	inspector := &fakeInspector{supported: true, inspect: func(_ context.Context, name string) (services.Unit, error) {
		if name != "web-alias.service" {
			t.Fatalf("inspected unexpected alias %q", name)
		}
		return services.Unit{Name: name, ID: "web.service"}, nil
	}}
	reader := &fakeJournalReader{supported: true, page: services.LogPage{Entries: []services.LogEntry{}}}
	handler := NewHandler(fixtureCollector{}, time.Second, testLogger(), WithServices(policy, inspector), WithServiceLogs(policy.LogReadPolicy(), reader))
	recorder := logRequest(handler, "GET", "/api/v1/services/web-alias.service/logs", logToken)
	var response struct {
		Data struct {
			Unit string `json:"unit"`
		} `json:"data"`
	}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil || response.Data.Unit != "web.service" || reader.unit != "web.service" {
		t.Fatalf("logs were not linked to canonical unit identity: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestServiceLogsResponseHasAnEncodedBodyLimit(t *testing.T) {
	entries := make([]services.LogEntry, services.MaxLogEntries)
	for i := range entries {
		entries[i] = services.LogEntry{
			Timestamp: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
			Message:   strings.Repeat("x", services.MaxLogMessageBytes),
			Cursor:    strings.Repeat("c", services.MaxJournalCursorBytes),
		}
	}
	reader := &fakeJournalReader{supported: true, page: services.LogPage{Entries: entries}}
	handler := logPolicyHandler(t, reader, []string{"web.service"}, []string{"web.service"})
	recorder := logRequest(handler, "GET", "/api/v1/services/web.service/logs", logToken)
	if recorder.Code != http.StatusServiceUnavailable || len(recorder.Body.Bytes()) > services.MaxLogResponseBytes || !strings.Contains(recorder.Body.String(), "logs_unavailable") {
		t.Fatalf("encoded response exceeded its limit: status=%d bytes=%d body=%s", recorder.Code, recorder.Body.Len(), recorder.Body.String()[:min(recorder.Body.Len(), 200)])
	}
}

type concurrentLogInspector struct {
	active  atomic.Int32
	maximum atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (i *concurrentLogInspector) Supported() bool { return true }
func (i *concurrentLogInspector) Inspect(_ context.Context, name string) (services.Unit, error) {
	active := i.active.Add(1)
	for maximum := i.maximum.Load(); active > maximum && !i.maximum.CompareAndSwap(maximum, active); maximum = i.maximum.Load() {
	}
	i.entered <- struct{}{}
	<-i.release
	i.active.Add(-1)
	return services.Unit{Name: name, ID: name}, nil
}

type staticJournalReader struct{}

func (staticJournalReader) Supported() bool { return true }
func (staticJournalReader) Read(context.Context, string, services.LogQuery) (services.LogPage, error) {
	return services.LogPage{Entries: []services.LogEntry{}}, nil
}

func TestServiceLogReadsApplyBoundedBackpressure(t *testing.T) {
	policy, err := services.NewPolicyWithLogs(serviceToken, nil, logToken, []string{"web.service"})
	if err != nil {
		t.Fatal(err)
	}
	inspector := &concurrentLogInspector{entered: make(chan struct{}, 5), release: make(chan struct{})}
	handler := NewHandler(fixtureCollector{}, 2*time.Second, testLogger(), WithServices(policy, inspector), WithServiceLogs(policy.LogReadPolicy(), staticJournalReader{}))
	results := make(chan *httptest.ResponseRecorder, 5)
	for range 5 {
		go func() { results <- logRequest(handler, "GET", "/api/v1/services/web.service/logs", logToken) }()
	}
	for range maxConcurrentServiceLogReads {
		select {
		case <-inspector.entered:
		case <-time.After(time.Second):
			close(inspector.release)
			t.Fatal("expected concurrent log request did not start")
		}
	}
	close(inspector.release)
	for range 5 {
		if recorder := <-results; recorder.Code != http.StatusOK {
			t.Fatalf("log request failed after backpressure: %d %s", recorder.Code, recorder.Body.String())
		}
	}
	if maximum := inspector.maximum.Load(); maximum > maxConcurrentServiceLogReads || maximum != maxConcurrentServiceLogReads {
		t.Fatalf("active service log requests peaked at %d", maximum)
	}
}

func urlQueryEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, ":", "%3A"), "+", "%2B")
}
