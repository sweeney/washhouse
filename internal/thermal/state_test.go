package thermal

import (
	"testing"
	"time"
)

func lastState(t *testing.T, tr *Tracker, id string) State {
	t.Helper()
	return mustStatus(t, tr, id).State
}

// Unknown until the window spans PowerMinSpan, then cooling, with one
// tank_state_changed.
func TestUnknownUntilThePowerIsKnown(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	ev, _ := s.ramp(tr, left, t0, 4*time.Minute+30*time.Second, 30*time.Second, 50, -0.1)
	if st := lastState(t, tr, left); st != StateUnknown || len(ev) != 0 {
		t.Fatalf("state %s, events %+v; want unknown, none", st, ev)
	}
	ev, _ = s.ramp(tr, left, t0.Add(5*time.Minute), time.Minute, 30*time.Second, 50, -0.1)
	ch := ofType(ev, EventStateChanged)
	if lastState(t, tr, left) != StateCooling || len(ch) != 1 || ch[0].Prev != StateUnknown || ch[0].State != StateCooling ||
		!ch[0].At.Equal(t0.Add(5*time.Minute)) || ch[0].DeviceID != left || ch[0].PowerKW == nil {
		t.Fatalf("events %+v", ev)
	}
	if len(ofType(ev, EventHeatingStarted))+len(ofType(ev, EventHeatingEnded)) != 0 {
		t.Fatalf("heating events: %+v", ev)
	}
}

// Heating starts at HeatingOnKW and ends below HeatingOffKW; between them
// the state holds.
func TestHeatingThresholdsAndHysteresis(t *testing.T) {
	cfg := planConfig() // on 0.5, off 0.3
	cases := []struct {
		name   string
		first  float64 // kW for 15 min
		second float64 // then this for 15 min
		want   State
	}{
		{"just under on stays cooling", 0.49, 0.49, StateCooling},
		{"just over on heats", 0.0, 0.51, StateHeating},
		{"between, from cooling, stays cooling", 0.0, 0.4, StateCooling},
		{"between, from heating, stays heating", 3, 0.4, StateHeating},
		{"just over off, from heating, stays heating", 3, 0.31, StateHeating},
		{"just under off ends heating", 3, 0.29, StateCooling},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSynth(t)
			tr := newTracker(t, cfg)
			_, temp := s.ramp(tr, left, t0, 15*time.Minute, 30*time.Second, 40, c.first)
			s.ramp(tr, left, t0.Add(15*time.Minute), 15*time.Minute, 30*time.Second, temp, c.second)
			if got := lastState(t, tr, left); got != c.want {
				t.Fatalf("state %s, want %s (power %v)", got, c.want, *mustStatus(t, tr, left).PowerKW)
			}
		})
	}
}

// heating_started and heating_ended carry the event time and the power.
func TestHeatingEvents(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	ev1, temp := s.ramp(tr, right, t0, 15*time.Minute, 30*time.Second, 40, 0)
	ev2, temp := s.ramp(tr, right, t0.Add(15*time.Minute), 30*time.Minute, 30*time.Second, temp, 4)
	ev3, _ := s.ramp(tr, right, t0.Add(45*time.Minute), 30*time.Minute, 30*time.Second, temp, -0.3)
	if n := len(ofType(ev1, EventHeatingStarted)); n != 0 {
		t.Fatalf("heating while flat: %+v", ev1)
	}
	hs := ofType(ev2, EventHeatingStarted)
	if len(hs) != 1 || hs[0].PowerKW == nil || *hs[0].PowerKW < 0.5 || hs[0].DeviceID != right {
		t.Fatalf("heating_started: %+v", ev2)
	}
	// The window's slope crosses 0.5 kW after about 10% of the window.
	if d := hs[0].At.Sub(t0.Add(15 * time.Minute)); d <= 0 || d > 3*time.Minute {
		t.Fatalf("heating_started %v after the gain began", d)
	}
	sc := ofType(ev2, EventStateChanged)
	if len(sc) != 1 || sc[0].Prev != StateCooling || sc[0].State != StateHeating || !sc[0].At.Equal(hs[0].At) {
		t.Fatalf("state changes: %+v", sc)
	}
	he := ofType(ev3, EventHeatingEnded)
	if len(he) != 1 || he[0].PowerKW == nil || *he[0].PowerKW >= 0.3 {
		t.Fatalf("heating_ended: %+v", ev3)
	}
	if len(ofType(ev3, EventHeatingStarted)) != 0 {
		t.Fatalf("heating restarted: %+v", ev3)
	}
}

// A tank with no sweep for StaleAfter is unknown, and a heating episode
// ends with it.
func TestStaleIsUnknown(t *testing.T) {
	cfg := planConfig()
	s := newSynth(t)
	tr := newTracker(t, cfg)
	_, _ = s.ramp(tr, left, t0, 15*time.Minute, 30*time.Second, 40, 3)
	last := mustStatus(t, tr, left).At
	if ev := tr.Tick(last.Add(cfg.StaleAfter)); len(ev) != 0 || lastState(t, tr, left) != StateHeating {
		t.Fatalf("at StaleAfter: %s %+v, want still heating", lastState(t, tr, left), ev)
	}
	ev := tr.Tick(last.Add(cfg.StaleAfter + time.Second))
	if lastState(t, tr, left) != StateUnknown {
		t.Fatalf("state %s after StaleAfter, want unknown", lastState(t, tr, left))
	}
	he, sc := ofType(ev, EventHeatingEnded), ofType(ev, EventStateChanged)
	if len(he) != 1 || len(sc) != 1 || sc[0].State != StateUnknown || he[0].PowerKW != nil ||
		!he[0].At.Equal(last.Add(cfg.StaleAfter+time.Second)) {
		t.Fatalf("events %+v", ev)
	}
	// The other tank never had a sweep: unknown, and no event for it.
	for _, e := range ev {
		if e.DeviceID != left {
			t.Fatalf("event for %s: %+v", e.DeviceID, e)
		}
	}
	if lastState(t, tr, right) != StateUnknown {
		t.Fatal("right tank known without sweeps")
	}
}

// Fewer than MinProbes usable probes (after the excluded roles) is unknown.
func TestTooFewProbesIsUnknown(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	_, temp := s.ramp(tr, left, t0, 15*time.Minute, 30*time.Second, 40, 0)
	if lastState(t, tr, left) != StateCooling {
		t.Fatal("not cooling")
	}
	// lower and middle fail: upper is the only bulk probe left.
	ev, err := tr.Sweep(s.sweep(left, t0.Add(15*time.Minute), temp, 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if lastState(t, tr, left) != StateUnknown || len(ofType(ev, EventStateChanged)) != 1 {
		t.Fatalf("state %s, events %+v", lastState(t, tr, left), ev)
	}
}

// "Heating when power ≥ the threshold": a power exactly at HeatingOnKW
// heats. The power is measured first, then the identical input replayed
// with the threshold set to it.
func TestHeatingAtExactlyOn(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	s.ramp(tr, left, t0, 15*time.Minute, 30*time.Second, 40, 0.8)
	p := *mustStatus(t, tr, left).PowerKW
	cfg := planConfig()
	cfg.HeatingOnKW, cfg.HeatingOffKW = p, p
	tr = newTracker(t, cfg)
	s.ramp(tr, left, t0, 15*time.Minute, 30*time.Second, 40, 0.8)
	if st := mustStatus(t, tr, left); st.State != StateHeating || *st.PowerKW != p {
		t.Fatalf("at exactly %v kW: %s (power %v)", p, st.State, *st.PowerKW)
	}
}
