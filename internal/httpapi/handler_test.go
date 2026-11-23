package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/engine"
)

// gate is a fake auth middleware: only "Authorization: ok" gets in.
func gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "ok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var health = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"status":"ok"}`) })

func newTestHandler(t *testing.T, rec *Recorder) http.Handler {
	t.Helper()
	h, err := New(Config{Snapshot: rec.Snapshot, Health: health, Auth: gate, PublicURL: "https://washhouse.example"})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, path string, authed bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if authed {
		r.Header.Set("Authorization", "ok")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func showerRecorder(t *testing.T) *Recorder {
	t.Helper()
	rec := NewRecorder(5)
	_, _, raw := exampleBehaviour(t)
	rec.Behaviour(raw, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), 5)
	replayInto(t, showerFixture, rec, nil)
	return rec
}

func TestNewRefusesAMissingPart(t *testing.T) {
	rec := NewRecorder(1)
	for name, c := range map[string]Config{
		"Snapshot": {Health: health, Auth: gate},
		"Health":   {Snapshot: rec.Snapshot, Auth: gate},
		"Auth":     {Snapshot: rec.Snapshot, Health: health},
	} {
		if _, err := New(c); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// /healthz and /openapi.json are public; everything under /api/ needs the
// token.
func TestOnlyTheAPINeedsAuth(t *testing.T) {
	h := newTestHandler(t, showerRecorder(t))
	for _, p := range []string{"/healthz", "/openapi.json"} {
		if w := get(h, p, false); w.Code != http.StatusOK {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	for _, p := range []string{"/api/v1/state", "/api/v1/tanks/hot_water_cylinder_left", "/api/v1/draws", "/api/v1/model",
		"/api/v1/tanks/nope", "/api/v2/state"} {
		if w := get(h, p, false); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d", p, w.Code)
		}
		if w := get(h, p, true); w.Code == http.StatusUnauthorized {
			t.Errorf("%s with a token: %d", p, w.Code)
		}
	}
}

func TestStateIsThePublishedPayload(t *testing.T) {
	rec := showerRecorder(t)
	h := newTestHandler(t, rec)
	w := get(h, "/api/v1/state", true)
	if w.Code != http.StatusOK || w.Body.String() != string(rec.Snapshot().State) ||
		w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("%d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	w = get(h, "/api/v1/tanks/hot_water_cylinder_right", true)
	if w.Code != http.StatusOK || w.Body.String() != string(rec.Snapshot().Tanks["hot_water_cylinder_right"]) {
		t.Errorf("tank: %d %s", w.Code, w.Body.String())
	}
}

func TestUnknownTankIs404(t *testing.T) {
	w := get(newTestHandler(t, showerRecorder(t)), "/api/v1/tanks/nope", true)
	if w.Code != http.StatusNotFound || strings.TrimSpace(w.Body.String()) != `{"error":"no tank \"nope\""}` {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

func TestUnknownPathIsAJSON404(t *testing.T) {
	h := newTestHandler(t, showerRecorder(t))
	for _, p := range []string{"/nope", "/api/v1/nope"} {
		w := get(h, p, true)
		if w.Code != http.StatusNotFound || strings.TrimSpace(w.Body.String()) != `{"error":"not found"}` {
			t.Errorf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/state", nil)
	r.Header.Set("Authorization", "ok")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

// Before the first input there is no state to give: 503, not a state
// dated the zero time. The draws (none) and the model are there.
func TestBeforeAnyInput(t *testing.T) {
	rec := NewRecorder(5)
	_, _, raw := exampleBehaviour(t)
	rec.Behaviour(raw, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), 5)
	h := newTestHandler(t, rec)
	for _, p := range []string{"/api/v1/state", "/api/v1/tanks/hot_water_cylinder_left"} {
		w := get(h, p, true)
		if w.Code != http.StatusServiceUnavailable || strings.TrimSpace(w.Body.String()) != `{"error":"washhouse has had no input yet"}` {
			t.Errorf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	if w := get(h, "/api/v1/draws", true); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"draws":[]}` {
		t.Errorf("draws: %d %s", w.Code, w.Body.String())
	}
	if w := get(h, "/api/v1/model", true); w.Code != http.StatusOK {
		t.Errorf("model: %d", w.Code)
	}
}

func TestDraws(t *testing.T) {
	rec := showerRecorder(t)
	w := get(newTestHandler(t, rec), "/api/v1/draws", true)
	var body struct {
		Draws []struct {
			Ended    json.RawMessage `json:"ended"`
			Balanced json.RawMessage `json:"balanced"`
		} `json:"draws"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %v", w.Code, err)
	}
	s := rec.Snapshot()
	if len(body.Draws) != len(s.Draws) || len(body.Draws) == 0 {
		t.Fatalf("%d draws, want %d", len(body.Draws), len(s.Draws))
	}
	if string(body.Draws[0].Ended) != strings.TrimSpace(string(s.Draws[0].Ended)) {
		t.Errorf("ended %s\nwant %s", body.Draws[0].Ended, s.Draws[0].Ended)
	}
}

func TestModel(t *testing.T) {
	rec := showerRecorder(t)
	w := get(newTestHandler(t, rec), "/api/v1/model", true)
	var body struct {
		Behaviour struct {
			Tanks []struct {
				DeviceID string `json:"device_id"`
			} `json:"tanks"`
		} `json:"behaviour"`
		AppliedAt string `json:"applied_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Behaviour.Tanks) != 2 || body.AppliedAt != "2001-01-01T00:00:00.000Z" {
		t.Errorf("%+v", body)
	}
}

// A browser's preflight is answered before auth: it carries no token.
func TestPreflightNeedsNoToken(t *testing.T) {
	h := newTestHandler(t, showerRecorder(t))
	r := httptest.NewRequest(http.MethodOptions, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("%d %v", w.Code, w.Header())
	}
	if w := get(h, "/api/v1/state", true); w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("no CORS on a GET")
	}
}

func TestOpenAPIServesThePublicURL(t *testing.T) {
	w := get(newTestHandler(t, showerRecorder(t)), "/openapi.json", false)
	if !strings.Contains(w.Body.String(), `"url": "https://washhouse.example"`) || strings.Contains(w.Body.String(), "__PUBLIC_URL__") {
		t.Errorf("servers in %.300s", w.Body.String())
	}
}

// A draw not yet balanced is listed with balanced null.
func TestUnbalancedDrawIsNull(t *testing.T) {
	rec := NewRecorder(5)
	at := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := rec.After(engine.State{At: at}, []engine.Event{endedEvent("a", at)}); err != nil {
		t.Fatal(err)
	}
	w := get(newTestHandler(t, rec), "/api/v1/draws", true)
	if !strings.HasSuffix(strings.TrimSpace(w.Body.String()), `,"balanced":null}]}`) {
		t.Errorf("%s", w.Body.String())
	}
}
