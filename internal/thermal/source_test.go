package thermal

import (
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/wire/boiler"
)

const (
	on  = boiler.On
	off = boiler.Off
	unk = boiler.Unknown
)

// heated runs tank id flat for 15 min from t0 with the programmer at hw/ch
// from the start, then gaining 3 kW for 15 min, and returns the events.
func heated(t *testing.T, tr *Tracker, id string, hw, ch boiler.Switch) []Event {
	t.Helper()
	s := newSynth(t)
	ev := tr.Programmer(t0, hw, ch)
	e, temp := s.ramp(tr, id, t0, 15*time.Minute, 30*time.Second, 40, 0)
	ev = append(ev, e...)
	e, _ = s.ramp(tr, id, t0.Add(15*time.Minute), 15*time.Minute, 30*time.Second, temp, 3)
	return append(ev, e...)
}

func TestSourceFromTheProgrammer(t *testing.T) {
	cases := []struct {
		hw, ch boiler.Switch
		want   Source
	}{
		{on, off, SourceHW},
		{on, on, SourceHW},
		{off, on, SourceCH},
		{off, off, SourceNone},
		{unk, on, SourceNone}, // CH on, HW not known: can't say it's CH
		{unk, unk, SourceNone},
	}
	for _, c := range cases {
		t.Run(c.hw.String()+"/"+c.ch.String(), func(t *testing.T) {
			tr := newTracker(t, planConfig())
			ev := heated(t, tr, right, c.hw, c.ch)
			st := mustStatus(t, tr, right)
			if st.State != StateHeating || st.Source != c.want {
				t.Fatalf("state %s source %q, want heating %q", st.State, st.Source, c.want)
			}
			hs := ofType(ev, EventHeatingStarted)
			if len(hs) != 1 || hs[0].Source != c.want {
				t.Fatalf("heating_started %+v", hs)
			}
		})
	}
}

// The source is only for heating: a cooling tank has none, whatever the
// programmer says (a satisfied stat, or a CH call that heats nothing).
func TestNoSourceWhenNotHeating(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	tr.Programmer(t0, on, on)
	s.ramp(tr, left, t0, 30*time.Minute, 30*time.Second, 60, -0.1)
	if st := mustStatus(t, tr, left); st.State != StateCooling || st.Source != SourceNone {
		t.Fatalf("status %+v", st)
	}
}

// After the programmer switches off, heat still reaches the probes for a
// while: the source holds for SourceHold, then is null.
func TestSourceHoldsAfterSwitchOff(t *testing.T) {
	cfg := planConfig()
	for _, c := range []struct {
		name   string
		hw, ch boiler.Switch
		want   Source
	}{
		{"hw", on, off, SourceHW},
		{"ch", off, on, SourceCH},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSynth(t)
			tr := newTracker(t, cfg)
			heated(t, tr, right, c.hw, c.ch)
			offAt := t0.Add(30 * time.Minute)
			ev := tr.Programmer(offAt, off, off)
			if len(ev) != 0 || mustStatus(t, tr, right).Source != c.want {
				t.Fatalf("at switch-off: %+v, events %+v", mustStatus(t, tr, right), ev)
			}
			temp := 40 + 3/kwhPerK*0.25
			// Keep gaining past the hold; sweeps every 30 s from the switch-off.
			ev, _ = s.ramp(tr, right, offAt, cfg.SourceHold+time.Minute, 30*time.Second, temp, 3)
			sc := ofType(ev, EventStateChanged)
			if len(sc) != 1 || sc[0].Source != SourceNone || sc[0].State != StateHeating || sc[0].At.Sub(offAt) <= cfg.SourceHold {
				t.Fatalf("source changes %+v, want one to null just after %v", sc, cfg.SourceHold)
			}
			if sc[0].At.Sub(offAt) > cfg.SourceHold+30*time.Second {
				t.Fatalf("source dropped %v after switch-off", sc[0].At.Sub(offAt))
			}
		})
	}
}

// HW off with CH still on is CH's at once: the hold is only for both off.
func TestHWOffWithCHOnIsCH(t *testing.T) {
	tr := newTracker(t, planConfig())
	heated(t, tr, right, on, on)
	at := t0.Add(30 * time.Minute)
	ev := tr.Programmer(at, off, on)
	sc := ofType(ev, EventStateChanged)
	if len(sc) != 1 || sc[0].Source != SourceCH || sc[0].Prev != StateHeating || !sc[0].At.Equal(at) {
		t.Fatalf("events %+v", ev)
	}
	// ...and back to HW when HW comes on again.
	ev = tr.Programmer(at.Add(time.Second), on, on)
	if sc := ofType(ev, EventStateChanged); len(sc) != 1 || sc[0].Source != SourceHW {
		t.Fatalf("events %+v", ev)
	}
}

// heating_ended carries the episode's source, even when it ends while the
// tank shows drawing (here: its input goes stale during a draw).
func TestHeatingEndedCarriesTheEpisodeSource(t *testing.T) {
	cfg := planConfig()
	tr := newTracker(t, cfg)
	heated(t, tr, right, on, off)
	last := mustStatus(t, tr, right).At
	tr.MeteredDraw(last.Add(time.Second), true)
	ev := tr.Tick(last.Add(cfg.StaleAfter + time.Second))
	he := ofType(ev, EventHeatingEnded)
	if len(he) != 1 || he[0].Source != SourceHW {
		t.Fatalf("heating_ended %+v, want source hw", he)
	}
}
