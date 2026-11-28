package engine

import (
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/sweeney/common/wire/tanktemp"
	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/thermal"
)

// blind is tank id's sweep at at with every probe failed with code, as
// the right tank's were when its bus failed (sweeney/washhouse#17).
func blind(id string, at time.Time, code string) tanktemp.State {
	st := sweep(id, at, nil, nil, nil, nil, nil)
	for i := range st.Sensors {
		c := code
		st.Sensors[i].Error = &c
	}
	return st
}

// sweepEvery gives e a sweep of each tank every 30 s from from to to
// (inclusive), left at leftC, right from rightAt, and returns the events.
func sweepEvery(t *testing.T, e *Engine, from, to time.Time, leftC float64, rightAt func(time.Time) tanktemp.State) []Event {
	t.Helper()
	var ev []Event
	for at := from; !at.After(to); at = at.Add(30 * time.Second) {
		x, err := e.TankState(uniform(left, at, leftC))
		if err != nil {
			t.Fatal(err)
		}
		ev = append(ev, x...)
		if rightAt != nil {
			x, _ = e.TankState(rightAt(at)) // a blind sweep is refused: that is the point
			ev = append(ev, x...)
		}
	}
	return ev
}

func hot(at time.Time) tanktemp.State { return uniform(right, at, 60) }

func blindAt(code string) func(time.Time) tanktemp.State {
	return func(at time.Time) tanktemp.State { return blind(right, at, code) }
}

func faultCfg() Config {
	c := planConfig()
	c.TankFaultAfter, c.TankRestoreAfter = 5*time.Minute, 2*time.Minute
	return c
}

// With one cylinder blind the totals stay unknown, and partial gives the
// other's: a lower bound, naming the one missing.
func TestABlindTankGivesAPartial(t *testing.T) {
	e := newEngine(t, faultCfg())
	sweepEvery(t, e, t0, t0.Add(time.Minute), 60, hot)
	s := e.State()
	if s.Partial != nil || s.UsableL40 == nil {
		t.Fatalf("both known: partial %+v, usable %v", s.Partial, fm(s.UsableL40, "%.1f"))
	}
	t1 := t0.Add(90 * time.Second)
	sweepEvery(t, e, t1, t1, 60, blindAt("bus_low"))
	s = e.State()
	l := s.Tanks[0]
	if s.UsableL40 != nil || s.UsableL40Low != nil || s.StoredKWh != nil {
		t.Errorf("totals with a tank blind: %v %v %v", fm(s.UsableL40, "%.1f"), fm(s.UsableL40Low, "%.1f"), fm(s.StoredKWh, "%.2f"))
	}
	p := s.Partial
	if p == nil || len(p.Missing) != 1 || p.Missing[0] != right ||
		p.UsableL40 != *l.UsableL40 || p.UsableL40Low != *l.UsableL40Low || p.StoredKWh != *l.StoredKWh {
		t.Errorf("partial %+v, want the left tank's (%v, %v, %v) missing right", p, *l.UsableL40, *l.UsableL40Low, *l.StoredKWh)
	}
	r := s.Tanks[1]
	if r.Fault == nil || !r.Fault.Since.Equal(t1) || r.Fault.Reason != FaultProbes ||
		!maps.Equal(r.Fault.Errors, map[string]int{"bus_low": 5}) || r.Fault.Lost {
		t.Errorf("right's fault %+v, want probes since %v, bus_low ×5, not yet lost", r.Fault, t1)
	}
	if r.State != thermal.StateUnknown {
		t.Errorf("a blind tank's state is %s, want unknown at once", r.State)
	}
	if l.Fault != nil {
		t.Errorf("the left tank has a fault: %+v", l.Fault)
	}
}

// Neither tank known: no partial.
func TestNoPartialWhenNoTankIsKnown(t *testing.T) {
	e := newEngine(t, faultCfg())
	e.TankState(blind(left, t0, "bus_low"))
	e.TankState(blind(right, t0, "bus_low"))
	if p := e.State().Partial; p != nil {
		t.Errorf("partial %+v with both tanks blind", p)
	}
}

// low_hot_water from a partial: false when the known tanks alone clear
// the level plus the margin (and the held value clears); unknown, with the
// held value kept, otherwise; never true.
func TestLowHotWaterFromAPartial(t *testing.T) {
	t.Run("enough without the blind tank: false, and the hold clears", func(t *testing.T) {
		e := newEngine(t, faultCfg())
		sweepEvery(t, e, t0, t0.Add(time.Minute), 20, func(at time.Time) tanktemp.State { return uniform(right, at, 20) })
		if lw := e.State().LowHotWater; lw == nil || !*lw {
			t.Fatalf("both cold: low_hot_water %v, want true", lw)
		}
		// The right goes blind while both are cold (the hold kept: true),
		// then the left heats alone.
		t1 := t0.Add(90 * time.Second)
		e.TankState(blind(right, t1, "bus_low"))
		if _, err := e.TankState(uniform(left, t1, 20)); err != nil {
			t.Fatal(err)
		}
		if e.low == nil || !*e.low {
			t.Fatalf("right blind, left cold: the held value is %v, want true kept", e.low)
		}
		t1 = t1.Add(30 * time.Second)
		sweepEvery(t, e, t1, t1, 60, blindAt("bus_low"))
		if lw := e.State().LowHotWater; lw == nil || *lw {
			t.Fatalf("left hot alone, right blind: low_hot_water %v, want false", lw)
		}
		if e.low == nil || *e.low {
			t.Fatalf("the held value is %v, want false: a partial that clears the level clears the hold", e.low)
		}
		// Back with the right cold: the left alone is well above, so false holds.
		t2 := t1.Add(30 * time.Second)
		sweepEvery(t, e, t2, t2, 60, func(at time.Time) tanktemp.State { return uniform(right, at, 20) })
		if lw := e.State().LowHotWater; lw == nil || *lw {
			t.Errorf("both known again: low_hot_water %v, want false", lw)
		}
	})
	t.Run("not enough: unknown, never true, and the hold is kept", func(t *testing.T) {
		e := newEngine(t, faultCfg())
		sweepEvery(t, e, t0, t0.Add(time.Minute), 60, hot)
		if lw := e.State().LowHotWater; lw == nil || *lw {
			t.Fatalf("both hot: low_hot_water %v, want false", lw)
		}
		t1 := t0.Add(90 * time.Second)
		sweepEvery(t, e, t1, t1, 20, blindAt("bus_low"))
		if lw := e.State().LowHotWater; lw != nil {
			t.Errorf("left cold, right blind: low_hot_water %v, want unknown (the right may hold plenty)", *lw)
		}
		if e.low == nil || *e.low {
			t.Errorf("the held value is %v, want false kept for the right's return", e.low)
		}
	})
}

// tank_lost once the fault has lasted TankFaultAfter, once however its
// errors change; tank_restored once the tank has been estimated again for
// TankRestoreAfter, dated then, with how long it was blind.
func TestLostAndRestored(t *testing.T) {
	e := newEngine(t, faultCfg())
	sweepEvery(t, e, t0, t0.Add(time.Minute), 60, hot)
	t1 := t0.Add(90 * time.Second)
	ev := sweepEvery(t, e, t1, t1.Add(150*time.Second), 60, blindAt("bus_low"))
	ev = append(ev, sweepEvery(t, e, t1.Add(180*time.Second), t1.Add(10*time.Minute), 60, blindAt("not_present"))...)
	lost := ofType(ev, EventTankLost)
	if len(lost) != 1 {
		t.Fatalf("%d tank_lost, want 1", len(lost))
	}
	if x := lost[0]; x.At.Before(t1.Add(5*time.Minute)) || x.At.After(t1.Add(5*time.Minute+30*time.Second)) ||
		x.Lost.DeviceID != right || !x.Lost.Since.Equal(t1) || x.Lost.Reason != FaultProbes ||
		!maps.Equal(x.Lost.Errors, map[string]int{"not_present": 5}) {
		t.Errorf("tank_lost %+v at %v, want right since %v, at the first input from %v", x.Lost, x.At, t1, t1.Add(5*time.Minute))
	}
	r := e.State().Tanks[1].Fault
	if r == nil || !r.Lost || !r.Since.Equal(t1) || r.LostAt == nil || !r.LostAt.Equal(lost[0].At) {
		t.Errorf("right's fault %+v, want lost since %v, lost_at %v", r, t1, lost[0].At)
	}
	t2 := t1.Add(10*time.Minute + 30*time.Second)
	ev = sweepEvery(t, e, t2, t2, 60, hot)
	// Read again but not yet restored: its figures are back, so no
	// partial, but its fault holds, lost_at and all, until tank_restored
	// (statehouse's review of sweeney/common#18: a consumer mustn't see
	// the fault flap during the restore's wait).
	if s := e.State(); s.Partial != nil || s.UsableL40 == nil || s.Tanks[1].UsableL40 == nil {
		t.Errorf("first good sweep: partial %+v, usable %v", s.Partial, fm(s.UsableL40, "%.1f"))
	} else if f := s.Tanks[1].Fault; f == nil || f.LostAt == nil || !f.Since.Equal(t1) {
		t.Errorf("first good sweep: fault %+v, want it held, lost since %v", f, t1)
	}
	ev = append(ev, sweepEvery(t, e, t2.Add(30*time.Second), t2.Add(5*time.Minute), 60, hot)...)
	restored := ofType(ev, EventTankRestored)
	if len(restored) != 1 {
		t.Fatalf("%d tank_restored, want 1", len(restored))
	}
	if x := restored[0]; !x.At.Equal(t2.Add(2*time.Minute)) || x.Restored.DeviceID != right ||
		!x.Restored.LostSince.Equal(t1) || x.Restored.Duration != t2.Sub(t1) {
		t.Errorf("tank_restored %+v at %v, want at %v, lost since %v, blind %v", x.Restored, x.At, t2.Add(2*time.Minute), t1, t2.Sub(t1))
	}
	if n := len(ofType(ev, EventTankLost)); n != 0 {
		t.Errorf("%d more tank_lost", n)
	}
	if f := e.State().Tanks[1].Fault; f != nil {
		t.Errorf("after tank_restored: fault %+v, want none", f)
	}
}

// A blind spell shorter than TankFaultAfter sends nothing and leaves no
// fault behind.
func TestABriefBlipIsNotALoss(t *testing.T) {
	e := newEngine(t, faultCfg())
	sweepEvery(t, e, t0, t0.Add(time.Minute), 60, hot)
	t1 := t0.Add(90 * time.Second)
	ev := sweepEvery(t, e, t1, t1.Add(4*time.Minute), 60, blindAt("crc_mismatch"))
	ev = append(ev, sweepEvery(t, e, t1.Add(270*time.Second), t1.Add(20*time.Minute), 60, hot)...)
	if n, m := len(ofType(ev, EventTankLost)), len(ofType(ev, EventTankRestored)); n+m != 0 {
		t.Errorf("a 4-minute blip sent %d tank_lost, %d tank_restored", n, m)
	}
	if f := e.State().Tanks[1].Fault; f != nil {
		t.Errorf("fault after the blip: %+v", f)
	}
	// A later spell starts afresh: lost TankFaultAfter after its own start.
	t2 := t1.Add(20*time.Minute + 30*time.Second)
	ev = sweepEvery(t, e, t2, t2.Add(6*time.Minute), 60, blindAt("bus_low"))
	lost := ofType(ev, EventTankLost)
	if len(lost) != 1 || !lost[0].Lost.Since.Equal(t2) || lost[0].At.Before(t2.Add(5*time.Minute)) {
		t.Errorf("the next spell: tank_lost %+v, want one since %v, from %v", lost, t2, t2.Add(5*time.Minute))
	}
}

// A lost tank that flickers back and blind again stays lost: one pair of
// events, the restore counted from the last good run, lost_since the first.
func TestAFlickeringTankSendsOnePair(t *testing.T) {
	e := newEngine(t, faultCfg())
	sweepEvery(t, e, t0, t0.Add(time.Minute), 60, hot)
	t1 := t0.Add(90 * time.Second)
	ev := sweepEvery(t, e, t1, t1.Add(6*time.Minute), 60, blindAt("bus_low"))
	t2 := t1.Add(390 * time.Second)
	ev = append(ev, sweepEvery(t, e, t2, t2.Add(time.Minute), 60, hot)...) // good, but not for 2 min
	t3 := t2.Add(90 * time.Second)
	ev = append(ev, sweepEvery(t, e, t3, t3.Add(time.Minute), 60, blindAt("bus_low"))...)
	if f := e.State().Tanks[1].Fault; f == nil || !f.Lost || !f.Since.Equal(t1) {
		t.Errorf("blind again: fault %+v, want still lost since %v", f, t1)
	}
	t4 := t3.Add(90 * time.Second)
	ev = append(ev, sweepEvery(t, e, t4, t4.Add(5*time.Minute), 60, hot)...)
	lost, restored := ofType(ev, EventTankLost), ofType(ev, EventTankRestored)
	if len(lost) != 1 || len(restored) != 1 {
		t.Fatalf("%d tank_lost, %d tank_restored, want 1 each", len(lost), len(restored))
	}
	if x := restored[0]; !x.At.Equal(t4.Add(2*time.Minute)) || !x.Restored.LostSince.Equal(t1) || x.Restored.Duration != t4.Sub(t1) {
		t.Errorf("tank_restored %+v at %v, want at %v, lost since %v, blind %v", x.Restored, x.At, t4.Add(2*time.Minute), t1, t4.Sub(t1))
	}
}

// A tank whose sweeps stop is blind once it goes stale, since then; while
// tanktemp says it is offline, that is the reason.
func TestAStaleTankIsBlind(t *testing.T) {
	e := newEngine(t, faultCfg())
	sweepEvery(t, e, t0, t0.Add(time.Minute), 60, hot)
	sweepEvery(t, e, t0.Add(90*time.Second), t0.Add(4*time.Minute), 60, nil)
	f := e.State().Tanks[1].Fault
	if want := t0.Add(time.Minute + 2*time.Minute); f == nil || f.Reason != FaultStale || !f.Since.Equal(want) || len(f.Errors) != 0 {
		t.Fatalf("right stale: fault %+v, want stale since %v, no errors", f, want)
	}
	e.Status(tanktemp.Status{Online: false})
	if f := e.State().Tanks[1].Fault; f == nil || f.Reason != FaultTanktempOffline {
		t.Errorf("tanktemp offline: fault %+v, want tanktemp_offline", f)
	}
}

// A tank never heard from isn't blind at once: only once StaleAfter has
// passed since the engine started.
func TestANeverSeenTankIsBlindOnlyOnceStale(t *testing.T) {
	e := newEngine(t, faultCfg())
	sweepEvery(t, e, t0, t0.Add(time.Minute), 60, nil)
	if f := e.State().Tanks[1].Fault; f != nil {
		t.Errorf("a minute in, no sweep yet from right: fault %+v, want none", f)
	}
	sweepEvery(t, e, t0.Add(90*time.Second), t0.Add(3*time.Minute), 60, nil)
	if f := e.State().Tanks[1].Fault; f == nil || f.Reason != FaultStale || !f.Since.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("three minutes in: fault %+v, want stale since %v", f, t0.Add(2*time.Minute))
	}
}

// A heating tank that goes blind ends its spell at once, with its state
// unknown: not "heating" with nothing behind it until it goes stale.
func TestABlindTankStopsHeatingAtOnce(t *testing.T) {
	e := newEngine(t, faultCfg())
	at := t0
	for i := 0; i < 30; i++ {
		e.TankState(uniform(left, at, 50))
		e.TankState(uniform(right, at, 30+float64(i)*0.5)) // gaining fast
		at = at.Add(30 * time.Second)
	}
	if st := e.State().Tanks[1].State; st != thermal.StateHeating {
		t.Fatalf("right's state %s, want heating", st)
	}
	ev, _ := e.TankState(blind(right, at, "bus_low"))
	ended := ofType(ev, EventHeatingEnded)
	if len(ended) != 1 || !ended[0].At.Equal(at) || ended[0].Tank.State != thermal.StateUnknown {
		t.Errorf("heating_ended %+v, want one at %v with state unknown", ended, at)
	}
}

// A reload mid-fault carries it on: no second tank_lost, and the restore
// gives the first since.
func TestAReloadKeepsAFault(t *testing.T) {
	e := newEngine(t, faultCfg())
	sweepEvery(t, e, t0, t0.Add(time.Minute), 60, hot)
	t1 := t0.Add(90 * time.Second)
	sweepEvery(t, e, t1, t1.Add(6*time.Minute), 60, blindAt("bus_low"))
	n := newEngine(t, faultCfg())
	n.TakeFaults(e)
	t2 := t1.Add(390 * time.Second)
	ev := sweepEvery(t, n, t2, t2.Add(2*time.Minute), 60, blindAt("bus_low"))
	if f := n.State().Tanks[1].Fault; f == nil || !f.Lost || !f.Since.Equal(t1) {
		t.Errorf("after the reload: fault %+v, want lost since %v", f, t1)
	}
	// Before the new engine's first sweep of it, the fault keeps its errors
	// (tanktemp's review of #18).
	m := newEngine(t, faultCfg())
	m.TakeFaults(e)
	m.TankState(uniform(left, t2, 60))
	if f := m.State().Tanks[1].Fault; f == nil || !maps.Equal(f.Errors, map[string]int{"bus_low": 5}) {
		t.Errorf("after the reload, before a sweep of it: fault %+v, want bus_low ×5 kept", f)
	}
	t3 := t2.Add(150 * time.Second)
	ev = append(ev, sweepEvery(t, n, t3, t3.Add(3*time.Minute), 60, hot)...)
	if lost, restored := ofType(ev, EventTankLost), ofType(ev, EventTankRestored); len(lost) != 0 || len(restored) != 1 ||
		!restored[0].Restored.LostSince.Equal(t1) {
		t.Errorf("after the reload: %d tank_lost, restored %+v, want none and one since %v", len(lost), restored, t1)
	}
}

// The delays are required.
func TestFaultDelaysAreValidated(t *testing.T) {
	for _, c := range []func(*Config){
		func(c *Config) { c.TankFaultAfter = 0 },
		func(c *Config) { c.TankRestoreAfter = -time.Second },
	} {
		cfg := faultCfg()
		c(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%+v was accepted", cfg)
		}
	}
}

// At a start, a tank not yet heard from isn't missing: no partial (and no
// low_hot_water from one) until it has been unheard for StaleAfter.
func TestNoPartialForATankNotYetHeard(t *testing.T) {
	e := newEngine(t, faultCfg())
	if _, err := e.TankState(uniform(left, t0, 60)); err != nil {
		t.Fatal(err)
	}
	if s := e.State(); s.Partial != nil || s.LowHotWater != nil {
		t.Errorf("right not yet heard: partial %+v, low %v; want neither", s.Partial, s.LowHotWater)
	}
}

// right-bus-lost: the right tank's bus failing for 46 h, then reseated.
// The rows are tanktemp's table on sweeney/washhouse#17, from main's
// replay before this change: times are offsets from the fixture's start.
func TestFixtureRightBusLost(t *testing.T) {
	r := feed(t, newEngine(t, planConfig()), "right-bus-lost", feedOpts{})
	at := func(h, m, s int) time.Time {
		return r.start.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
	}
	onset, back := at(0, 30, 17), at(46, 53, 23)
	cfg := planConfig()

	// 2, 9: the right tank unknown, its heating ended, at the first failed
	// sweep; nothing more from its thermal state until it is back.
	var rightThermal []Event
	for _, e := range r.events {
		if e.Tank != nil && e.Tank.DeviceID == right && !e.At.Before(onset) && e.At.Before(back) {
			rightThermal = append(rightThermal, e)
		}
	}
	if len(rightThermal) == 0 || !rightThermal[0].At.Equal(onset) || rightThermal[0].Tank.State != thermal.StateUnknown {
		t.Errorf("the right tank's thermal events in the spell: %+v; want unknown at %v", rightThermal, onset)
	}
	for _, e := range rightThermal {
		if !e.At.Equal(onset) {
			t.Errorf("+%v: %s for the right tank while it is blind", r.ofs(e.At), e.Type)
		}
	}

	var blindStates, partials int
	minLow, maxLow := 1e9, 0.0
	seenBack := false
	for i, s := range r.states {
		rt, lt := s.Tanks[1], s.Tanks[0]
		// The spell runs by the right tank's own sweeps: in a sweep, the
		// left's state can come first, with the right's last still good.
		rAt := r.start
		if rt.At != nil {
			rAt = *rt.At
		}
		switch {
		case rAt.Before(onset):
			if s.Partial != nil {
				t.Fatalf("+%v: a partial before the onset", r.ofs(s.At))
			}
		case rAt.Before(back):
			// 3, 6, 8: blind: totals null, partial the left's, low false.
			blindStates++
			if rt.Fault == nil || !rt.Fault.Since.Equal(onset) {
				t.Fatalf("+%v: the right tank's fault %+v, want since %v", r.ofs(s.At), rt.Fault, r.ofs(onset))
			}
			if s.UsableL40 != nil || s.StoredKWh != nil {
				t.Fatalf("+%v: totals with the right tank blind", r.ofs(s.At))
			}
			if lt.UsableL40Low == nil {
				continue // the left mid-sweep (its own stale edge): nothing to compare
			}
			p := s.Partial
			if p == nil || len(p.Missing) != 1 || p.Missing[0] != right || p.UsableL40Low != *lt.UsableL40Low ||
				p.UsableL40 != *lt.UsableL40 || p.StoredKWh != *lt.StoredKWh {
				t.Fatalf("+%v: partial %+v, want the left tank's", r.ofs(s.At), p)
			}
			partials++
			minLow, maxLow = min(minLow, p.UsableL40Low), max(maxLow, p.UsableL40Low)
			if s.LowHotWater == nil || *s.LowHotWater {
				t.Fatalf("+%v: low_hot_water %v with %.1f L known, want false", r.ofs(s.At), s.LowHotWater, p.UsableL40Low)
			}
		case !seenBack:
			// 11: the first state with the right tank back.
			seenBack = true
			if s.Partial != nil || rt.Fault == nil || rt.Fault.LostAt == nil || s.UsableL40 == nil || math.Abs(*s.UsableL40-239.3) > 0.5 ||
				s.LowHotWater == nil || *s.LowHotWater {
				t.Errorf("+%v (state %d): back: partial %+v, fault %+v, usable %s, low %v; want 239.3 L, not low, the fault held until restored",
					r.ofs(s.At), i, s.Partial, rt.Fault, fm(s.UsableL40, "%.1f"), s.LowHotWater)
			}
		}
	}
	t.Logf("%d blind states, %d with a partial; its usable_l40_low %.1f–%.1f L", blindStates, partials, minLow, maxLow)
	// 7: the left tank's range, which clears the level throughout.
	if partials < 5000 || math.Abs(minLow-189.4) > 0.5 || math.Abs(maxLow-314.2) > 0.5 ||
		!(minLow > cfg.LowHotWaterL+cfg.LowHotWaterMarginL) {
		t.Errorf("%d partials, usable_l40_low %.1f–%.1f; want 189.4–314.2", partials, minLow, maxLow)
	}

	// 5, 12, 14: one loss, one restore, both the right tank's, while the
	// fixture runs (the replay's closing tick, minutes past its last input,
	// rightly finds both tanks stale).
	end := r.states[len(r.states)-1].At
	var inFixture []Event
	for _, e := range r.events {
		if !e.At.After(end) {
			inFixture = append(inFixture, e)
		}
	}
	lost, restored := ofType(inFixture, EventTankLost), ofType(inFixture, EventTankRestored)
	if len(lost) != 1 || len(restored) != 1 {
		t.Fatalf("%d tank_lost, %d tank_restored; want 1 each", len(lost), len(restored))
	}
	if l := lost[0]; l.Lost.DeviceID != right || !l.Lost.Since.Equal(onset) || l.Lost.Reason != FaultProbes ||
		l.At.Before(onset.Add(cfg.TankFaultAfter)) || l.At.After(onset.Add(cfg.TankFaultAfter+time.Minute)) {
		t.Errorf("tank_lost at +%v: %+v", r.ofs(l.At), l.Lost)
	}
	if x := restored[0]; x.Restored.DeviceID != right || !x.Restored.LostSince.Equal(onset) ||
		x.Restored.Duration != 166986*time.Second ||
		x.At.Before(back.Add(cfg.TankRestoreAfter)) || x.At.After(back.Add(cfg.TankRestoreAfter+time.Minute)) {
		t.Errorf("tank_restored at +%v: %+v, blind %v", r.ofs(x.At), x.Restored, x.Restored.Duration)
	}

	// 15: the left tank is never blind.
	for _, s := range r.states {
		if s.Tanks[0].Fault != nil && s.At.After(at(0, 5, 0)) {
			t.Fatalf("+%v: the left tank has a fault %+v", r.ofs(s.At), s.Tanks[0].Fault)
		}
	}

	// 16 (provisional: the left tank's own share is a follow-up): the
	// draws in the spell are attributed to neither tank.
	for _, b := range r.balanced {
		if b.EndedAt.After(onset) && b.EndedAt.Before(back) && !slices.Contains(b.Reasons, draw.ReasonTankMissing) {
			t.Errorf("draw %s in the spell without tank_missing: %v", b.DrawID, b.Reasons)
		}
	}
}

// A room reading doesn't start the engine's clock: statehouse's retained
// one, dated by the sensor's last report, can be half an hour old at a
// start, and a tank not yet heard from would look blind at once
// (sweeney/washhouse#19).
func TestAnOldRoomReadingDoesNotStartTheClock(t *testing.T) {
	e := newEngine(t, faultCfg())
	e.Ambient(t0.Add(-30*time.Minute), 21)
	if _, err := e.TankState(uniform(left, t0, 60)); err != nil {
		t.Fatal(err)
	}
	if f := e.State().Tanks[1].Fault; f != nil {
		t.Errorf("right not yet heard, a moment after the start: fault %+v, want none", f)
	}
	if a := e.State().AmbientC; a == nil || *a != 21 {
		t.Errorf("the room reading, 30 min old: %v, want 21 (ambient_stale is 40 min)", fm(a, "%v"))
	}
}

// An old room reading first, then tanktemp's first sweep: the clock starts
// at the sweep, so a draw UAQuiet − 1 min later doesn't count as following
// a rest, and teaches no outlet correction (tanktemp's review of #21).
func TestAnOldRoomReadingDoesNotMakeARest(t *testing.T) {
	cfg := faultCfg()
	e := newEngine(t, cfg)
	e.Ambient(t0.Add(-35*time.Minute), 21)
	e.TankState(uniform(left, t0, 60))
	e.TankState(uniform(right, t0, 60))
	if !e.started.Equal(t0) {
		t.Fatalf("started %v, want tanktemp's first sweep %v", e.started, t0)
	}
	d := meter.Draw{ID: "d-1", Start: t0.Add(cfg.Thermal.UAQuiet - time.Minute)}
	e.drawStarts(d)
	if r := e.outletRest[d.ID]; len(r) != 0 {
		t.Errorf("a draw %v after the start counted as rested: %v", cfg.Thermal.UAQuiet-time.Minute, r)
	}
	d = meter.Draw{ID: "d-2", Start: t0.Add(cfg.Thermal.UAQuiet)}
	e.drawStarts(d)
	if r := e.outletRest[d.ID]; len(r) != 2 {
		t.Errorf("a draw UAQuiet after the start: rest %v, want both tanks'", r)
	}
}
