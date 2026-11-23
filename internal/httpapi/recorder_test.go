package httpapi

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/sweeney/common/wire/washhouse"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/publish"
)

const showerFixture = "../../testdata/fixtures/shower-layered.log.gz"

func TestRecorderBeforeAnyInput(t *testing.T) {
	r := NewRecorder(5)
	if s := r.Snapshot(); s == nil || s.State != nil || len(s.Draws) != 0 {
		t.Errorf("snapshot %+v", s)
	}
}

// The state and tanks are the published payloads, byte for byte.
func TestRecorderKeepsThePayloads(t *testing.T) {
	r := NewRecorder(5)
	replayInto(t, showerFixture, r, nil)
	s := r.Snapshot()
	var st washhouse.State
	if err := json.Unmarshal(s.State, &st); err != nil || st.Schema != washhouse.SchemaState {
		t.Fatalf("state %s: %v", s.State, err)
	}
	if len(s.Tanks) != 2 || len(s.TankIDs) != 2 || s.TankIDs[0] != "hot_water_cylinder_left" {
		t.Fatalf("tanks %v", s.TankIDs)
	}
	if _, err := washhouse.DecodeTank(s.Tanks["hot_water_cylinder_right"]); err != nil {
		t.Error(err)
	}
}

// The shower's draw is listed from draw_ended, with draw_balanced once
// that comes: the events as published.
func TestRecorderListsTheDraws(t *testing.T) {
	r := NewRecorder(5)
	var sawUnbalanced bool
	replayInto(t, showerFixture, r, func(s *Snapshot) {
		if len(s.Draws) > 0 && s.Draws[0].Balanced == nil {
			sawUnbalanced = true
		}
	})
	s := r.Snapshot()
	if len(s.Draws) == 0 {
		t.Fatal("no draws")
	}
	if !sawUnbalanced {
		t.Error("a draw was never listed before its draw_balanced")
	}
	for _, d := range s.Draws {
		e, err := washhouse.DecodeEvent(d.Ended)
		if err != nil || e.Event != washhouse.EventDrawEnded {
			t.Fatalf("ended %s: %v", d.Ended, err)
		}
		if d.Balanced == nil {
			continue
		}
		b, err := washhouse.DecodeEvent(d.Balanced)
		if err != nil || b.DrawBalanced.DrawID != e.DrawEnded.DrawID {
			t.Errorf("balanced %s for %s: %v", d.Balanced, e.DrawEnded.DrawID, err)
		}
	}
	if s.Draws[0].Balanced == nil {
		t.Error("the last draw never balanced")
	}
}

func endedEvent(id string, at time.Time) engine.Event {
	return engine.Event{Type: engine.EventDrawEnded, At: at, Ended: &engine.DrawEnded{DrawID: id, StartedAt: at, EndedAt: at,
		Litres: 1, VolumeKind: "metered", Kind: "tap"}}
}

// Newest first, at most keep; a draw_balanced for a draw no longer kept
// is dropped.
func TestRecorderKeepsTheNewest(t *testing.T) {
	r := NewRecorder(2)
	at := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	st := engine.State{At: at}
	for i, id := range []string{"a", "b", "c"} {
		if err := r.After(st, []engine.Event{endedEvent(id, at.Add(time.Duration(i)*time.Minute))}); err != nil {
			t.Fatal(err)
		}
	}
	ids := func() []string {
		var out []string
		for _, d := range r.Snapshot().Draws {
			e, _ := washhouse.DecodeEvent(d.Ended)
			out = append(out, e.DrawEnded.DrawID)
		}
		return out
	}
	if got := ids(); len(got) != 2 || got[0] != "c" || got[1] != "b" {
		t.Fatalf("draws %v", got)
	}
	bal := engine.Event{Type: engine.EventDrawBalanced, At: at, Balanced: &engine.DrawBalanced{DrawID: "a", EndedAt: at}}
	if err := r.After(st, []engine.Event{bal}); err != nil {
		t.Fatal(err)
	}
	for _, d := range r.Snapshot().Draws {
		if d.Balanced != nil {
			t.Errorf("a balance for a dropped draw was kept: %s", d.Balanced)
		}
	}
	bal.Balanced.DrawID = "b"
	if err := r.After(st, []engine.Event{bal}); err != nil {
		t.Fatal(err)
	}
	if d := r.Snapshot().Draws[1]; d.Balanced == nil {
		t.Error("b's balance wasn't kept")
	}
	want, _ := publish.Event(bal, "x")
	if got := r.Snapshot().Draws[1].Balanced; string(got) != string(want.Payload) {
		t.Errorf("balanced %s\nwant %s", got, want.Payload)
	}
}

// A snapshot handed out never changes: the API reads it while the loop
// moves on.
func TestSnapshotsAreImmutable(t *testing.T) {
	r := NewRecorder(5)
	at := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := r.After(engine.State{At: at}, []engine.Event{endedEvent("a", at)}); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()
	bal := engine.Event{Type: engine.EventDrawBalanced, At: at, Balanced: &engine.DrawBalanced{DrawID: "a", EndedAt: at}}
	if err := r.After(engine.State{At: at, Tanks: []engine.TankState{{DeviceID: "t", State: "unknown"}}}, []engine.Event{bal}); err != nil {
		t.Fatal(err)
	}
	if len(s.Draws) != 1 || s.Draws[0].Balanced != nil || len(s.Tanks) != 0 || len(s.TankIDs) != 0 {
		t.Errorf("an old snapshot changed: %+v", s)
	}
	if err := r.After(engine.State{At: at}, []engine.Event{endedEvent("b", at)}); err != nil {
		t.Fatal(err)
	}
	if len(s.Draws) != 1 {
		t.Errorf("an old snapshot changed: %+v", s.Draws)
	}
}

// The behaviour and its time; keep follows the document.
func TestRecorderBehaviour(t *testing.T) {
	r := NewRecorder(5)
	at := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b", "c"} {
		_ = r.After(engine.State{At: at}, []engine.Event{endedEvent(id, at.Add(time.Duration(i)*time.Minute))})
	}
	r.Behaviour([]byte(`{"x":1}`), at, 1)
	s := r.Snapshot()
	if string(s.Behaviour) != `{"x":1}` || !s.AppliedAt.Equal(at) || len(s.Draws) != 1 {
		t.Errorf("snapshot %+v", s)
	}
}

// A trimmed list has no spare capacity: a snapshot's slice can never be
// grown into another's (tanktemp's review of #7).
func TestTrimmedDrawsHaveNoSpareCapacity(t *testing.T) {
	r := NewRecorder(2)
	at := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b", "c"} {
		_ = r.After(engine.State{At: at}, []engine.Event{endedEvent(id, at.Add(time.Duration(i)*time.Minute))})
	}
	if d := r.Snapshot().Draws; cap(d) != len(d) {
		t.Errorf("len %d, cap %d", len(d), cap(d))
	}
	r.Behaviour(nil, at, 1)
	if d := r.Snapshot().Draws; cap(d) != len(d) {
		t.Errorf("after Behaviour: len %d, cap %d", len(d), cap(d))
	}
}

// After that fails changes nothing: a later Behaviour doesn't store a
// half-made snapshot (tanktemp's review of #7).
func TestFailedAfterChangesNothing(t *testing.T) {
	r := NewRecorder(5)
	at := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	good := engine.State{At: at, Tanks: []engine.TankState{{DeviceID: "t", State: "unknown"}}}
	if err := r.After(good, []engine.Event{endedEvent("a", at)}); err != nil {
		t.Fatal(err)
	}
	before := r.Snapshot()
	// The state and the tanks encode; the draw doesn't (NaN litres), so
	// After fails after they are made.
	next := engine.State{At: at.Add(time.Minute), Tanks: []engine.TankState{{DeviceID: "t", State: "unknown"}, {DeviceID: "u", State: "unknown"}}}
	badDraw := endedEvent("b", at)
	badDraw.Ended.Litres = math.NaN()
	if err := r.After(next, []engine.Event{badDraw}); err == nil {
		t.Fatal("a draw of NaN litres encoded")
	}
	r.Behaviour([]byte(`{}`), at, 5)
	s := r.Snapshot()
	if string(s.State) != string(before.State) || len(s.Tanks) != 1 || s.Tanks["t"] == nil || len(s.TankIDs) != 1 || len(s.Draws) != 1 {
		t.Errorf("after a failed After: %d tanks %v, %d draws", len(s.Tanks), s.TankIDs, len(s.Draws))
	}
}
