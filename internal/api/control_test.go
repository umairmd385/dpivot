package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	dpivotapi "github.com/dpivot/dpivot/internal/api"
	"github.com/dpivot/dpivot/internal/metrics"
	"github.com/dpivot/dpivot/internal/proxy"
	"go.uber.org/zap"
)

func newTestAPI(t *testing.T) (*proxy.Registry, *httptest.Server) {
	t.Helper()
	m := metrics.New()
	reg := proxy.NewRegistry()
	router := proxy.NewRouter(reg)
	srv := proxy.NewServer(router, zap.NewNop(), m)
	t.Cleanup(srv.Close)

	cs := dpivotapi.NewControlServer(reg, srv, zap.NewNop(), m)
	ts := httptest.NewServer(cs.Handler())
	t.Cleanup(ts.Close)
	return reg, ts
}

// ── /health ───────────────────────────────────────────────────────────────────

func TestAPI_Health(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health: status %d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body) //nolint:errcheck
	if body["status"] != "ok" {
		t.Errorf("health.status = %v, want ok", body["status"])
	}
}

func TestAPI_Health_WrongMethod(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := http.Post(ts.URL+"/health", "application/json", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("health POST: want 405, got %d", resp.StatusCode)
	}
}

// ── /health/live ──────────────────────────────────────────────────────────────

func TestAPI_HealthLive(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, err := http.Get(ts.URL + "/health/live")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health/live: status %d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body) //nolint:errcheck
	if body["status"] != "ok" {
		t.Errorf("health/live.status = %v, want ok", body["status"])
	}
}

// ── /health/ready ─────────────────────────────────────────────────────────────

func TestAPI_HealthReady_NoBackends_Returns503(t *testing.T) {
	_, ts := newTestAPI(t) // empty registry
	resp, err := http.Get(ts.URL + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("health/ready with no backends: want 503, got %d", resp.StatusCode)
	}
}

func TestAPI_HealthReady_WithBackend_Returns200(t *testing.T) {
	reg, ts := newTestAPI(t)
	reg.Add(proxy.Backend{ID: "b1", Addr: "10.0.0.1:8080"}) //nolint:errcheck
	resp, err := http.Get(ts.URL + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health/ready with backend: want 200, got %d", resp.StatusCode)
	}
}

func TestAPI_HealthReady_DrainingOnly_Returns503(t *testing.T) {
	reg, ts := newTestAPI(t)
	reg.Add(proxy.Backend{ID: "b1", Addr: "10.0.0.1:8080"}) //nolint:errcheck
	reg.SetDraining("b1")                                    //nolint:errcheck
	resp, _ := http.Get(ts.URL + "/health/ready")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("health/ready draining-only: want 503, got %d", resp.StatusCode)
	}
}

// ── /metrics ──────────────────────────────────────────────────────────────────

func TestAPI_Metrics_Returns200(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("metrics: want 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		t.Error("metrics: expected Content-Type header")
	}
}

// ── /backends ─────────────────────────────────────────────────────────────────

func TestAPI_ListBackends_Empty(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := http.Get(ts.URL + "/backends")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("list backends: want 200, got %d", resp.StatusCode)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body) //nolint:errcheck
	if body["count"].(float64) != 0 {
		t.Errorf("count = %v, want 0", body["count"])
	}
}

func TestAPI_AddBackend_Created(t *testing.T) {
	_, ts := newTestAPI(t)
	payload := `{"id":"b1","addr":"10.0.0.1:8080"}`
	resp, err := http.Post(ts.URL+"/backends", "application/json", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("add backend: want 201, got %d", resp.StatusCode)
	}
}

func TestAPI_AddBackend_AutoID(t *testing.T) {
	_, ts := newTestAPI(t)
	payload := `{"addr":"10.0.0.1:8080"}`
	resp, _ := http.Post(ts.URL+"/backends", "application/json", bytes.NewBufferString(payload))
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("auto-id: want 201, got %d", resp.StatusCode)
	}
}

func TestAPI_AddBackend_DuplicateID_Conflict(t *testing.T) {
	_, ts := newTestAPI(t)
	payload := `{"id":"b1","addr":"10.0.0.1:8080"}`
	http.Post(ts.URL+"/backends", "application/json", bytes.NewBufferString(payload)) //nolint:errcheck
	resp, _ := http.Post(ts.URL+"/backends", "application/json", bytes.NewBufferString(payload))
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate: want 409, got %d", resp.StatusCode)
	}
}

func TestAPI_AddBackend_MissingAddr(t *testing.T) {
	_, ts := newTestAPI(t)
	payload := `{"id":"b1"}`
	resp, _ := http.Post(ts.URL+"/backends", "application/json", bytes.NewBufferString(payload))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing addr: want 400, got %d", resp.StatusCode)
	}
}

func TestAPI_AddBackend_InvalidJSON(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := http.Post(ts.URL+"/backends", "application/json", bytes.NewBufferString("{bad json}"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid json: want 400, got %d", resp.StatusCode)
	}
}

func TestAPI_AddAndList(t *testing.T) {
	_, ts := newTestAPI(t)
	http.Post(ts.URL+"/backends", "application/json", bytes.NewBufferString(`{"id":"b1","addr":"10.0.0.1:8080"}`)) //nolint:errcheck
	http.Post(ts.URL+"/backends", "application/json", bytes.NewBufferString(`{"id":"b2","addr":"10.0.0.2:8080"}`)) //nolint:errcheck

	resp, _ := http.Get(ts.URL + "/backends")
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body) //nolint:errcheck
	if body["count"].(float64) != 2 {
		t.Errorf("count = %v, want 2", body["count"])
	}
}

func TestAPI_RemoveBackend(t *testing.T) {
	_, ts := newTestAPI(t)
	http.Post(ts.URL+"/backends", "application/json", bytes.NewBufferString(`{"id":"b1","addr":"10.0.0.1:8080"}`)) //nolint:errcheck

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/backends/b1", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("remove: want 204, got %d", resp.StatusCode)
	}
}

func TestAPI_RemoveUnknown_NotFound(t *testing.T) {
	_, ts := newTestAPI(t)
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/backends/nobody", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("remove unknown: want 404, got %d", resp.StatusCode)
	}
}

func TestAPI_DrainBackend(t *testing.T) {
	_, ts := newTestAPI(t)
	http.Post(ts.URL+"/backends", "application/json", bytes.NewBufferString(`{"id":"b1","addr":"10.0.0.1:8080"}`)) //nolint:errcheck

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/backends/b1/drain", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("drain: want 204, got %d", resp.StatusCode)
	}
}

func TestAPI_DrainUnknown_NotFound(t *testing.T) {
	_, ts := newTestAPI(t)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/backends/ghost/drain", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("drain unknown: want 404, got %d", resp.StatusCode)
	}
}

func TestAPI_WrongMethod_BackendsList(t *testing.T) {
	_, ts := newTestAPI(t)
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/backends", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PATCH /backends: want 405, got %d", resp.StatusCode)
	}
}

func TestAPI_HealthCountReflectsRegistry(t *testing.T) {
	_, ts := newTestAPI(t)
	http.Post(ts.URL+"/backends", "application/json", bytes.NewBufferString(`{"id":"b1","addr":"10.0.0.1:8080"}`)) //nolint:errcheck
	resp, _ := http.Get(ts.URL + "/health")
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body) //nolint:errcheck
	if body["backends"].(float64) != 1 {
		t.Errorf("health.backends = %v, want 1", body["backends"])
	}
}
