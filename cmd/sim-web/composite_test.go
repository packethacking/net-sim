package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testApp(recordBase string) *app {
	return &app{
		recordBase: recordBase,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func decodeComposite(t *testing.T, body io.Reader) compositeStatus {
	t.Helper()
	var cs compositeStatus
	if err := json.NewDecoder(body).Decode(&cs); err != nil {
		t.Fatalf("decode composite status: %v", err)
	}
	return cs
}

func TestCompositeStatusUnavailableWithoutRecordDir(t *testing.T) {
	a := testApp("")
	rec := httptest.NewRecorder()
	a.handleCompositeStatus(rec, httptest.NewRequest(http.MethodGet, "/api/record/composite/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	cs := decodeComposite(t, rec.Body)
	if cs.Available {
		t.Error("Available should be false when no -record dir")
	}
	if cs.Active {
		t.Error("Active should be false")
	}
}

func TestCompositeStatusAvailableWithRecordDir(t *testing.T) {
	a := testApp(t.TempDir())
	rec := httptest.NewRecorder()
	a.handleCompositeStatus(rec, httptest.NewRequest(http.MethodGet, "/api/record/composite/status", nil))
	cs := decodeComposite(t, rec.Body)
	if !cs.Available {
		t.Error("Available should be true when -record dir set")
	}
}

func TestCompositeStartRejectedWhenRecordingDisabled(t *testing.T) {
	a := testApp("")
	rec := httptest.NewRecorder()
	a.handleCompositeStart(rec, httptest.NewRequest(http.MethodPost, "/api/record/composite/start", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", rec.Code)
	}
}

func TestCompositeStartRejectedWhenRouterStopped(t *testing.T) {
	a := testApp(t.TempDir())
	rec := httptest.NewRecorder()
	a.handleCompositeStart(rec, httptest.NewRequest(http.MethodPost, "/api/record/composite/start", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400 (router not running)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not running") {
		t.Errorf("body = %q, want mention of router not running", rec.Body.String())
	}
}

func TestCompositeStartRejectsBadJSON(t *testing.T) {
	a := testApp(t.TempDir())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/record/composite/start", strings.NewReader("{not json"))
	a.handleCompositeStart(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400 (bad json)", rec.Code)
	}
}

func TestCompositeStartRejectsBadPortRef(t *testing.T) {
	a := testApp(t.TempDir())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/record/composite/start",
		strings.NewReader(`{"ports":["nodotsep"]}`))
	a.handleCompositeStart(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400 (bad port ref)", rec.Code)
	}
}

func TestCompositeStartWrongMethod(t *testing.T) {
	a := testApp(t.TempDir())
	rec := httptest.NewRecorder()
	a.handleCompositeStart(rec, httptest.NewRequest(http.MethodGet, "/api/record/composite/start", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status code = %d, want 405", rec.Code)
	}
}

func TestCompositeDownloadNotFoundWhenNoFile(t *testing.T) {
	a := testApp(t.TempDir())
	rec := httptest.NewRecorder()
	a.handleCompositeDownload(rec, httptest.NewRequest(http.MethodGet, "/api/record/composite/download", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want 404", rec.Code)
	}
}

func TestStatusIncludesCompositeBlock(t *testing.T) {
	a := testApp(t.TempDir())
	rec := httptest.NewRecorder()
	a.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	comp, ok := resp["composite"].(map[string]any)
	if !ok {
		t.Fatalf("status JSON missing composite object; got %v", resp)
	}
	if comp["available"] != true {
		t.Errorf("composite.available = %v, want true", comp["available"])
	}
}

func TestParsePortRefs(t *testing.T) {
	refs, err := parsePortRefs([]string{"a.vhf", " b.uhf-link ", ""})
	if err != nil {
		t.Fatalf("parsePortRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2 (empty skipped)", len(refs))
	}
	if refs[0].NodeID != "a" || refs[0].PortID != "vhf" {
		t.Errorf("refs[0] = %+v, want a/vhf", refs[0])
	}
	if refs[1].NodeID != "b" || refs[1].PortID != "uhf-link" {
		t.Errorf("refs[1] = %+v, want b/uhf-link", refs[1])
	}
	for _, bad := range []string{"nodot", ".leading", "trailing."} {
		if _, err := parsePortRefs([]string{bad}); err == nil {
			t.Errorf("parsePortRefs(%q) should error", bad)
		}
	}
}
