// Package httpapi is washhouse's HTTP API (openapi.yaml): the state and
// the tanks as published, recent draws as their events, and the running
// behaviour. The service's loop records each input's outcome (Recorder);
// the handlers serve the latest snapshot, so they never touch the engine.
package httpapi

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/sweeney/common/web/serve"
	"github.com/sweeney/common/web/spec"
	"github.com/sweeney/common/wire/washhouse"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/publish"
)

//go:embed openapi.yaml
var openapiYAML []byte

// Routes are the API's paths, as the spec names them.
var Routes = []string{"/healthz", "/openapi.json", "/api/v1/state", "/api/v1/tanks/{id}", "/api/v1/draws", "/api/v1/model"}

// Snapshot is what the API serves. One handed out never changes.
type Snapshot struct {
	State     []byte            // {base}/state's payload; nil before the first input
	Tanks     map[string][]byte // device_id → {base}/tank/{id}'s payload
	TankIDs   []string          // in the behaviour's order
	Draws     []Draw            // newest first
	Behaviour []byte            // the running document
	AppliedAt time.Time
}

// Draw is a recent draw: its draw_ended event, and its draw_balanced once
// that has come (nil before).
type Draw struct {
	ID       string
	Ended    json.RawMessage
	Balanced json.RawMessage
}

// Recorder makes the snapshots. After and Behaviour are the loop's;
// Snapshot is anyone's.
type Recorder struct {
	keep int
	cur  Snapshot // the loop's working copy
	snap atomic.Pointer[Snapshot]
}

// NewRecorder keeps at most keep draws.
func NewRecorder(keep int) *Recorder {
	r := &Recorder{keep: keep}
	r.store()
	return r
}

func (r *Recorder) Snapshot() *Snapshot { return r.snap.Load() }

// store hands out the working snapshot. It shares the slices and the map
// with what came before, so they are never written in place: After makes
// new ones.
func (r *Recorder) store() {
	s := r.cur
	r.snap.Store(&s)
}

// After records an input's outcome: st, and ev's draw events. It builds
// the new snapshot apart and takes it only when all of it encoded, so a
// failure changes nothing.
func (r *Recorder) After(st engine.State, ev []engine.Event) error {
	state, err := publish.State(st)
	if err != nil {
		return err
	}
	tanks, ids := map[string][]byte{}, []string(nil)
	for _, t := range st.Tanks {
		tb, err := publish.Tank(st, t)
		if err != nil {
			return err
		}
		tanks[t.DeviceID] = tb
		ids = append(ids, t.DeviceID)
	}
	draws := r.cur.Draws
	for _, e := range ev {
		switch {
		case e.Type == engine.EventDrawEnded && e.Ended != nil:
			m, err := publish.Event(e, "")
			if err != nil {
				return err
			}
			draws = append([]Draw{{ID: e.Ended.DrawID, Ended: m.Payload}}, draws...)
		case e.Type == engine.EventDrawBalanced && e.Balanced != nil:
			m, err := publish.Event(e, "")
			if err != nil {
				return err
			}
			for i := range draws {
				if draws[i].ID == e.Balanced.DrawID {
					d := draws[i] // a new Draw in a new slice: snapshots keep theirs
					d.Balanced = m.Payload
					draws = append(append(append([]Draw{}, draws[:i]...), d), draws[i+1:]...)
					break
				}
			}
		}
	}
	r.cur.State, r.cur.Tanks, r.cur.TankIDs, r.cur.Draws = state, tanks, ids, draws
	r.trim()
	r.store()
	return nil
}

// Behaviour records the running document, when it was applied, and how
// many draws it keeps.
func (r *Recorder) Behaviour(raw []byte, at time.Time, keep int) {
	r.cur.Behaviour, r.cur.AppliedAt, r.keep = raw, at, keep
	r.trim()
	r.store()
}

// trim keeps the newest keep draws, clipped: no spare capacity for a
// later append to write into a slice a snapshot holds.
func (r *Recorder) trim() {
	if len(r.cur.Draws) > r.keep {
		r.cur.Draws = slices.Clip(r.cur.Draws[:r.keep])
	}
}

// Config is what the handler needs.
type Config struct {
	Snapshot  func() *Snapshot
	Health    http.Handler                    // /healthz
	Auth      func(http.Handler) http.Handler // around /api/
	PublicURL string                          // the spec's server; "" leaves the placeholder
}

// New is the API's handler: CORS around everything, Auth around /api/.
func New(c Config) (http.Handler, error) {
	switch {
	case c.Snapshot == nil:
		return nil, errors.New("httpapi: Snapshot is required")
	case c.Health == nil:
		return nil, errors.New("httpapi: Health is required")
	case c.Auth == nil:
		return nil, errors.New("httpapi: Auth is required")
	}
	doc := openapiYAML
	if c.PublicURL != "" {
		doc = bytes.ReplaceAll(doc, []byte("__PUBLIC_URL__"), []byte(c.PublicURL))
	}
	conv := spec.NewConverter(doc)
	if _, err := conv.JSON(); err != nil {
		return nil, fmt.Errorf("httpapi: the spec: %w", err)
	}

	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/state", func(w http.ResponseWriter, _ *http.Request) {
		s := c.Snapshot()
		if s.State == nil {
			notReady(w)
			return
		}
		raw(w, s.State)
	})
	api.HandleFunc("GET /api/v1/tanks/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, id := c.Snapshot(), r.PathValue("id")
		if s.State == nil {
			notReady(w)
			return
		}
		b, ok := s.Tanks[id]
		if !ok {
			serve.WriteError(w, http.StatusNotFound, fmt.Sprintf("no tank %q", id))
			return
		}
		raw(w, b)
	})
	api.HandleFunc("GET /api/v1/draws", func(w http.ResponseWriter, _ *http.Request) {
		type draw struct {
			Ended    json.RawMessage `json:"ended"`
			Balanced json.RawMessage `json:"balanced"`
		}
		out := struct {
			Draws []draw `json:"draws"`
		}{Draws: []draw{}}
		for _, d := range c.Snapshot().Draws {
			b := d.Balanced
			if b == nil {
				b = json.RawMessage("null")
			}
			out.Draws = append(out.Draws, draw{Ended: d.Ended, Balanced: b})
		}
		serve.WriteJSON(w, http.StatusOK, out)
	})
	api.HandleFunc("GET /api/v1/model", func(w http.ResponseWriter, _ *http.Request) {
		s := c.Snapshot()
		serve.WriteJSON(w, http.StatusOK, struct {
			Behaviour json.RawMessage `json:"behaviour"`
			AppliedAt washhouse.Time  `json:"applied_at"`
		}{s.Behaviour, washhouse.Time{Time: s.AppliedAt}})
	})
	api.HandleFunc("GET /api/", func(w http.ResponseWriter, _ *http.Request) {
		serve.WriteError(w, http.StatusNotFound, "not found")
	})

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", c.Health)
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		b, _ := conv.JSON()
		raw(w, b)
	})
	mux.Handle("/api/", c.Auth(api))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		serve.WriteError(w, http.StatusNotFound, "not found")
	})
	return serve.CORS(mux), nil
}

// notReady is the 503 before the first input. Retry-After is about one
// tanktemp sweep: every 503 carries one (openapi.yaml).
func notReady(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "30")
	serve.WriteError(w, http.StatusServiceUnavailable, "washhouse has had no input yet")
}

// raw writes an already-encoded JSON body.
func raw(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}
