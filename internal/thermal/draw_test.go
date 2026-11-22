package thermal

import (
	"testing"
	"time"
)

// A metered draw makes every tank drawing from its start until DrawSettle
// after its end.
func TestDrawingWhileMeteredAndSettling(t *testing.T) {
	cfg := planConfig()
	s := newSynth(t)
	tr := newTracker(t, cfg)
	_, tl := s.ramp(tr, left, t0, 15*time.Minute, 30*time.Second, 50, -0.1)
	_, tr2 := s.ramp(tr, right, t0, 15*time.Minute, 30*time.Second, 40, -0.1)
	start := t0.Add(15 * time.Minute)
	ev := tr.MeteredDraw(start, true)
	sc := ofType(ev, EventStateChanged)
	if len(sc) != 2 || sc[0].State != StateDrawing || sc[1].State != StateDrawing || !sc[0].At.Equal(start) {
		t.Fatalf("draw start: %+v", ev)
	}
	// The draw takes heat out fast; the power says so.
	ev, _ = s.ramp(tr, left, start, 5*time.Minute, 10*time.Second, tl, -20)
	_, _ = s.ramp(tr, right, start, 5*time.Minute, 10*time.Second, tr2, -20)
	if len(ev) != 0 {
		t.Fatalf("events during the draw: %+v", ev)
	}
	st := mustStatus(t, tr, left)
	if st.State != StateDrawing || st.Source != SourceNone || st.PowerKW == nil || *st.PowerKW > -5 {
		t.Fatalf("during the draw: %+v", st)
	}
	end := start.Add(5 * time.Minute)
	if ev := tr.MeteredDraw(end, false); len(ev) != 0 {
		t.Fatalf("draw end, inside settle: %+v", ev)
	}
	if ev := tr.Tick(end.Add(cfg.DrawSettle)); len(ev) != 0 || lastState(t, tr, left) != StateDrawing {
		t.Fatalf("at the end of settle: %s %+v", lastState(t, tr, left), ev)
	}
	ev = tr.Tick(end.Add(cfg.DrawSettle + time.Second))
	sc = ofType(ev, EventStateChanged)
	if len(sc) != 2 || sc[0].State != StateCooling || sc[0].Prev != StateDrawing {
		t.Fatalf("after settle: %+v", ev)
	}
}

// A draw before the tank's power is known is still drawing.
func TestDrawingBeforePowerIsKnown(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	s.ramp(tr, left, t0, time.Minute, 30*time.Second, 50, 0)
	tr.MeteredDraw(t0.Add(time.Minute), true)
	if st := lastState(t, tr, left); st != StateDrawing {
		t.Fatalf("state %s", st)
	}
}

// A stale tank is unknown even while a draw runs.
func TestStaleBeatsDrawing(t *testing.T) {
	cfg := planConfig()
	s := newSynth(t)
	tr := newTracker(t, cfg)
	s.ramp(tr, left, t0, 15*time.Minute, 30*time.Second, 50, 0)
	last := mustStatus(t, tr, left).At
	tr.MeteredDraw(last.Add(time.Second), true)
	tr.Tick(last.Add(cfg.StaleAfter + time.Second))
	if st := lastState(t, tr, left); st != StateUnknown {
		t.Fatalf("state %s", st)
	}
}

// A draw during a reheat: drawing while it runs, then heating again, with no
// heating_ended/started in between, although the power window still holds
// the draw's loss for a while after it.
func TestDrawDuringAReheatHoldsHeating(t *testing.T) {
	cfg := planConfig()
	s := newSynth(t)
	tr := newTracker(t, cfg)
	tr.Programmer(t0, on, off)
	_, c := s.ramp(tr, left, t0, 20*time.Minute, 30*time.Second, 40, 3)
	if lastState(t, tr, left) != StateHeating {
		t.Fatal("not heating")
	}
	start := t0.Add(20 * time.Minute)
	var ev []Event
	ev = append(ev, tr.MeteredDraw(start, true)...)
	e, c := s.ramp(tr, left, start, 4*time.Minute, 10*time.Second, c, -15)
	ev = append(ev, e...)
	end := start.Add(4 * time.Minute)
	ev = append(ev, tr.MeteredDraw(end, false)...)
	e, _ = s.ramp(tr, left, end, 30*time.Minute, 30*time.Second, c, 3)
	ev = append(ev, e...)
	if n := len(ofType(ev, EventHeatingEnded)) + len(ofType(ev, EventHeatingStarted)); n != 0 {
		t.Fatalf("heating events across the draw: %+v", ev)
	}
	var states []State
	for _, e := range ofType(ev, EventStateChanged) {
		states = append(states, e.State)
	}
	if len(states) != 2 || states[0] != StateDrawing || states[1] != StateHeating {
		t.Fatalf("states %v, want drawing then heating", states)
	}
	if st := mustStatus(t, tr, left); st.State != StateHeating || st.Source != SourceHW {
		t.Fatalf("after: %+v", st)
	}
}

// After a draw, a window that still holds the draw doesn't decide heating:
// the fitting or mixing gain after it isn't a reheat.
func TestNoHeatingJudgedFromADrawWindow(t *testing.T) {
	cfg := planConfig()
	s := newSynth(t)
	tr := newTracker(t, cfg)
	_, c := s.ramp(tr, left, t0, 15*time.Minute, 30*time.Second, 50, -0.1)
	start := t0.Add(15 * time.Minute)
	tr.MeteredDraw(start, true)
	// The probes lag the meter: the drop lands late, and the window then
	// shows a steep rise from its trough.
	_, c = s.ramp(tr, left, start, 2*time.Minute, 10*time.Second, c, 0)
	end := start.Add(2 * time.Minute)
	tr.MeteredDraw(end, false)
	_, c = s.ramp(tr, left, end, 30*time.Second, 10*time.Second, c, -40)
	ev, _ := s.ramp(tr, left, end.Add(30*time.Second), cfg.PowerWindow, 10*time.Second, c, 2)
	if hs := ofType(ev, EventHeatingStarted); len(hs) != 0 {
		t.Fatalf("heating judged from a window with the draw in it: %+v", hs)
	}
	// Once the window is clear of the draw, a real gain is heating.
	ev, _ = s.ramp(tr, left, end.Add(30*time.Second+cfg.PowerWindow), 5*time.Minute, 10*time.Second, c+2/kwhPerK*cfg.PowerWindow.Hours(), 2)
	if hs := ofType(ev, EventHeatingStarted); len(hs) != 1 {
		t.Fatalf("no heating once clear: %+v", ev)
	}
}
