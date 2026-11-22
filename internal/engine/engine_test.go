package engine

import (
	"math"
	"sort"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/wire/boiler"
	"github.com/sweeney/washhouse/internal/wire/tanktemp"
)

func bp(b bool) *bool { return &b }

func boolStr(p *bool) string {
	switch {
	case p == nil:
		return "nil"
	case *p:
		return "on"
	}
	return "off"
}

// The programmer's HW and CH are unknown until an event or a ready
// heartbeat says; a RECONNECTED, a last will, a STARTUP or a heartbeat not
// yet ready changes nothing.
func TestBoilerTriState(t *testing.T) {
	e := newEngine(t, planConfig())
	check := func(step, hw, ch string) {
		t.Helper()
		b := e.State().Boiler
		if boolStr(b.HW) != hw || boolStr(b.CH) != ch {
			t.Fatalf("%s: hw %s ch %s, want hw %s ch %s", step, boolStr(b.HW), boolStr(b.CH), hw, ch)
		}
	}
	check("start", "nil", "nil")
	at := t0
	next := func() time.Time { at = at.Add(time.Minute); return at }

	e.BoilerSystem(boiler.System{Shape: boiler.ShapeSystem, Event: boiler.SystemReconnected, Timestamp: next()})
	check("RECONNECTED first", "nil", "nil")
	e.BoilerSystem(boiler.System{Shape: boiler.ShapeStatus, Event: boiler.SystemHeartbeat, Timestamp: next(),
		HW: boiler.On, CH: boiler.On, Ready: bp(false)})
	check("heartbeat not ready", "nil", "nil")
	e.BoilerSystem(boiler.System{Shape: boiler.ShapeStatus, Event: boiler.SystemHeartbeat, Timestamp: next(),
		HW: boiler.On, CH: boiler.On})
	check("heartbeat with no ready", "nil", "nil")
	e.BoilerSystem(boiler.System{Shape: boiler.ShapeStatus, Event: boiler.SystemStartup, Timestamp: next(),
		HW: boiler.On, CH: boiler.On, Ready: bp(true)})
	check("STARTUP", "nil", "nil")
	e.BoilerSystem(boiler.System{Shape: boiler.ShapeStatus, Event: boiler.SystemHeartbeat, Timestamp: next(),
		HW: boiler.On, CH: boiler.Off, Ready: bp(true)})
	check("ready heartbeat", "on", "off")
	e.BoilerSystem(boiler.System{Shape: boiler.ShapeSystem, Event: boiler.SystemReconnected, Timestamp: next()})
	check("RECONNECTED after", "on", "off")
	e.BoilerSystem(boiler.System{Shape: boiler.ShapeSystem, Event: boiler.SystemShutdown, Reason: "MQTT_DISCONNECT",
		Source: "last_will", Timestamp: t0.Add(-19 * 24 * time.Hour)})
	check("last will", "on", "off")
	e.BoilerEvent(boiler.Event{Timestamp: next(), Event: boiler.EventCHOn, HW: boiler.On, CH: boiler.On})
	check("CH_ON", "on", "on")
	e.BoilerEvent(boiler.Event{Timestamp: next(), Event: boiler.EventHWOff, HW: boiler.Off, CH: boiler.On})
	check("HW_OFF", "off", "on")
	e.BoilerSystem(boiler.System{Shape: boiler.ShapeStatus, Event: boiler.SystemHeartbeat, Timestamp: next(),
		HW: boiler.Unknown, CH: boiler.Off, Ready: bp(true)})
	check("heartbeat with HW unknown", "nil", "off")
	if got := e.State().At; !got.Equal(at) {
		t.Errorf("state at %v, want %v (the last-will's old timestamp must not move time back)", got, at)
	}
}

// Before any input everything is unknown: nil, never 0.
func TestNothingKnownIsNil(t *testing.T) {
	e := newEngine(t, planConfig())
	s := e.State()
	if s.StoredKWh != nil || s.UsableL40 != nil || s.UsableL40Low != nil || s.LowHotWater != nil ||
		s.AmbientC != nil || s.ActiveDraw != nil || s.Boiler.HW != nil || s.Boiler.CH != nil {
		t.Errorf("system state %+v: want every value nil", s)
	}
	m := s.Meter
	if m.At != nil || m.Count != nil || m.CountingSince != nil || m.Litres != nil || m.ContinuousFlow != nil {
		t.Errorf("meter %+v: want nil", m)
	}
	if s.Tanktemp.At != nil || s.Tanktemp.Online != nil || s.Tanktemp.Version != nil {
		t.Errorf("tanktemp %+v: want nil", s.Tanktemp)
	}
	if len(s.Tanks) != 2 || s.Tanks[0].DeviceID != left || s.Tanks[1].DeviceID != right {
		t.Fatalf("tanks %+v, want left and right in config order", s.Tanks)
	}
	for _, tk := range s.Tanks {
		if tk.At != nil || tk.StoredKWh != nil || tk.MeanC != nil || tk.TopC != nil || tk.HotL != nil ||
			tk.UsableL40 != nil || tk.UsableL40Low != nil || tk.PowerKW != nil || tk.ProbesOK != nil ||
			tk.ProbesTotal != nil || tk.HeatSource != nil || tk.State != "unknown" {
			t.Errorf("tank %+v: want unknown and nil", tk)
		}
	}
}

// A cold tank's estimate is a known 0, not unknown; a stale tank's is
// unknown again, and so are the totals.
func TestKnownZeroAndStale(t *testing.T) {
	e := newEngine(t, planConfig())
	for _, id := range []string{left, right} {
		if _, err := e.TankState(uniform(id, t0, 10)); err != nil {
			t.Fatal(err)
		}
	}
	s := e.State()
	for _, tk := range s.Tanks {
		if tk.StoredKWh == nil || *tk.StoredKWh != 0 || tk.UsableL40 == nil || *tk.UsableL40 != 0 ||
			tk.HotL == nil || *tk.HotL != 0 || tk.MeanC == nil || tk.TopC == nil || *tk.TopC != 10 {
			t.Errorf("cold tank %+v: want known zeros", tk)
		}
		if tk.At == nil || !tk.At.Equal(t0) || tk.ProbesOK == nil || *tk.ProbesOK != 5 || tk.ProbesTotal == nil || *tk.ProbesTotal != 5 {
			t.Errorf("tank %s: at %v probes %v/%v", tk.DeviceID, tk.At, tk.ProbesOK, tk.ProbesTotal)
		}
		if tk.PowerKW != nil {
			t.Errorf("tank %s: power %v from one sweep, want nil", tk.DeviceID, *tk.PowerKW)
		}
		if tk.UAWPerK == nil {
			t.Errorf("tank %s: no UA (the prior is known)", tk.DeviceID)
		}
	}
	if s.UsableL40Low == nil || *s.UsableL40Low != 0 || s.LowHotWater == nil || !*s.LowHotWater {
		t.Errorf("totals %v, low %v: want a known 0, and low", fm(s.UsableL40Low, "%v"), s.LowHotWater)
	}
	// The left tank goes on reporting; the right falls silent.
	stale := planConfig().Thermal.StaleAfter
	for at := t0.Add(30 * time.Second); !at.After(t0.Add(stale + time.Minute)); at = at.Add(30 * time.Second) {
		if _, err := e.TankState(uniform(left, at, 60)); err != nil {
			t.Fatal(err)
		}
	}
	s = e.State()
	if r := s.Tanks[1]; r.StoredKWh != nil || r.UsableL40Low != nil || r.State != "unknown" {
		t.Errorf("stale right tank %+v: want its estimate nil", r)
	}
	if r := s.Tanks[1]; r.At == nil || !r.At.Equal(t0) {
		t.Errorf("stale right tank at %v, want its last sweep's time", r.At)
	}
	if l := s.Tanks[0]; l.StoredKWh == nil || *l.StoredKWh <= 0 {
		t.Errorf("left tank %+v: want its estimate", l)
	}
	if s.StoredKWh != nil || s.UsableL40Low != nil || s.LowHotWater != nil {
		t.Errorf("totals with a stale tank: %v %v %v, want nil", s.StoredKWh, s.UsableL40Low, s.LowHotWater)
	}
}

// A sweep the model can't estimate (one usable probe) is an error, and the
// tank's estimate is unknown; one from an unconfigured tank is an error.
func TestUnusableSweep(t *testing.T) {
	e := newEngine(t, planConfig())
	if _, err := e.TankState(uniform(left, t0, 50)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.TankState(sweep(left, t0.Add(30*time.Second), f(50), nil, nil, nil, nil)); err == nil {
		t.Error("a one-probe sweep: no error")
	}
	if tk := e.State().Tanks[0]; tk.StoredKWh != nil || tk.ProbesOK == nil || *tk.ProbesOK != 1 {
		t.Errorf("after an unusable sweep: %+v, want the estimate nil and 1 probe ok", tk)
	}
	if _, err := e.TankState(uniform("hot_water_cylinder_middle", t0.Add(time.Minute), 50)); err == nil {
		t.Error("an unconfigured tank: no error")
	}
}

// A sweep with an unsynced clock is skipped, as is one no later than the
// tank's last.
func TestSkippedSweeps(t *testing.T) {
	e := newEngine(t, planConfig())
	if _, err := e.TankState(uniform(left, t0, 50)); err != nil {
		t.Fatal(err)
	}
	un := uniform(left, t0.Add(time.Hour), 20)
	un.ClockSynced = false
	old := uniform(left, t0, 20)
	for _, st := range []tanktemp.State{un, old} {
		if _, err := e.TankState(st); err != nil {
			t.Fatal(err)
		}
	}
	s := e.State()
	if l := s.Tanks[0]; !l.At.Equal(t0) || math.Abs(*l.MeanC-50) > 1e-3 {
		t.Errorf("left %v at %v, want the first sweep kept", *l.MeanC, l.At)
	}
	if !s.At.Equal(t0) {
		t.Errorf("state at %v: an unsynced sweep moved time", s.At)
	}
}

// low_hot_water is the system's: the summed usable_l40_low against the
// threshold, below it and not at it, whatever one tank holds alone.
func TestLowHotWaterBoundary(t *testing.T) {
	feed := func(cfg Config) State {
		e := newEngine(t, cfg)
		// The left tank hot at the top, the right nearly spent.
		if _, err := e.TankState(sweep(left, t0, f(15), f(20), f(45), f(50), f(55))); err != nil {
			t.Fatal(err)
		}
		if _, err := e.TankState(sweep(right, t0, f(15), f(15), f(20), f(38), f(41))); err != nil {
			t.Fatal(err)
		}
		return e.State()
	}
	s := feed(planConfig())
	total := *s.UsableL40Low
	l, r := *s.Tanks[0].UsableL40Low, *s.Tanks[1].UsableL40Low
	t.Logf("usable_l40_low: left %.1f L, right %.1f L, total %.1f L", l, r, total)
	if !(r < l) || r <= 0 {
		t.Fatalf("fixture: right %.1f L should be the smaller and above 0", r)
	}
	for _, tc := range []struct {
		name      string
		threshold float64
		want      bool
	}{
		{"at the total", total, false},
		{"just above the total", total + 1e-9, true},
		{"between the tanks", (l + r) / 2, false},
		{"above the right alone, below the total", r + 1e-9, false},
		{"zero", 0, false},
	} {
		cfg := planConfig()
		cfg.LowHotWaterL = tc.threshold
		got := feed(cfg).LowHotWater
		if got == nil || *got != tc.want {
			t.Errorf("%s (%.3f L): low %v, want %v", tc.name, tc.threshold, boolStr(got), tc.want)
		}
	}
}

func TestAmbientAndStatus(t *testing.T) {
	e := newEngine(t, planConfig())
	e.Ambient(t0, 24.5)
	if s := e.State(); s.AmbientC == nil || *s.AmbientC != 24.5 || !s.At.Equal(t0) {
		t.Errorf("ambient %v at %v", s.AmbientC, s.At)
	}
	ts := t0.Add(time.Minute)
	e.Status(tanktemp.Status{Online: true, Event: tanktemp.EventHeartbeat, Timestamp: &ts, Version: "fixture", Ready: true})
	s := e.State()
	if s.Tanktemp.Online == nil || !*s.Tanktemp.Online || s.Tanktemp.Version == nil || *s.Tanktemp.Version != "fixture" ||
		s.Tanktemp.At == nil || !s.Tanktemp.At.Equal(ts) {
		t.Errorf("tanktemp %+v", s.Tanktemp)
	}
	e.Status(tanktemp.Status{Online: false, Event: tanktemp.EventLWT})
	s = e.State()
	if s.Tanktemp.Online == nil || *s.Tanktemp.Online || s.Tanktemp.Version != nil || s.Tanktemp.At != nil {
		t.Errorf("after the last will: %+v, want offline with no version or time", s.Tanktemp)
	}
	// The room reading is unknown once older than AmbientStale.
	e.Tick(t0.Add(planConfig().Thermal.AmbientStale))
	if s := e.State(); s.AmbientC == nil {
		t.Error("ambient unknown at exactly AmbientStale")
	}
	e.Tick(t0.Add(planConfig().Thermal.AmbientStale + time.Second))
	if s := e.State(); s.AmbientC != nil {
		t.Errorf("ambient %v after AmbientStale, want nil", *s.AmbientC)
	}
}

// meterSim makes meter messages as tanktemp's WIRE.md describes them: a
// pulse on each closing, an open when the reed opens, numbered by seq.
type meterSim struct {
	since time.Time
	seq   uint64
	count uint64
}

func (m *meterSim) msg(at time.Time, tr tanktemp.Trigger, lv tanktemp.Level) tanktemp.Meter {
	m.seq++
	return tanktemp.Meter{DeviceID: "hot_water_meter", Timestamp: at.Truncate(time.Second), ClockSynced: true,
		Trigger: tr, Seq: m.seq, LitresPerPulse: 1, Level: lv, CountingSince: m.since, Count: m.count}
}

// litre is one litre closing at at, held closed for closed.
func (m *meterSim) litre(at time.Time, closed time.Duration) []tanktemp.Meter {
	m.count++
	p := m.msg(at, tanktemp.TriggerPulse, tanktemp.LevelClosed)
	lp := at
	p.LastPulseAt = &lp
	o := m.msg(at.Add(closed), tanktemp.TriggerOpen, tanktemp.LevelOpen)
	o.LastPulseAt = &lp
	ms := closed.Milliseconds()
	o.LastClosedMS = &ms
	return []tanktemp.Meter{p, o}
}

// draw feeds n litres every gap from start into e and returns the events.
func (m *meterSim) draw(e *Engine, start time.Time, n int, gap time.Duration) []Event {
	var ev []Event
	for i := 0; i < n; i++ {
		for _, x := range m.litre(start.Add(time.Duration(i)*gap), 2*time.Second) {
			ev = append(ev, e.Meter(x)...)
		}
	}
	return ev
}

// A running draw is in the state with draw_started's id and start; it
// leaves once the meter ends it; draw_ended follows when the attribution
// is complete.
func TestActiveDrawMatchesDrawStarted(t *testing.T) {
	e := newEngine(t, planConfig())
	m := &meterSim{since: t0.Add(-time.Hour)}
	e.Meter(m.msg(t0, tanktemp.TriggerSweep, tanktemp.LevelOpen)) // the baseline
	start := t0.Add(time.Minute)
	ev := m.draw(e, start, 5, 10*time.Second)
	st := ofType(ev, EventDrawStarted)
	if len(st) != 1 {
		t.Fatalf("draw_started %d, want 1: %+v", len(st), ev)
	}
	a := e.State().ActiveDraw
	if a == nil || a.DrawID != st[0].Draw.DrawID || !a.StartedAt.Equal(st[0].Draw.StartedAt) || !st[0].At.Equal(start) ||
		!a.StartedAt.Equal(start) || a.LitresSoFar != 5 || a.FlowLPerMin == nil || *a.FlowLPerMin != 6 {
		t.Fatalf("active %+v, draw_started %+v at %v", a, st[0].Draw, st[0].At)
	}
	if want := meter.DrawID(meter.Ref{CountingSince: m.since, Seq: 2}); a.DrawID != want {
		t.Errorf("draw id %s, want %s", a.DrawID, want)
	}
	if s := e.State(); s.Meter.Count == nil || *s.Meter.Count != 5 || s.Meter.Litres == nil || *s.Meter.Litres != 5 ||
		s.Meter.CountingSince == nil || !s.Meter.CountingSince.Equal(m.since) {
		t.Errorf("meter %+v", s.Meter)
	}
	last := start.Add(40 * time.Second)
	ev = e.Tick(last.Add(planConfig().Meter.DrawEnd))
	if e.State().ActiveDraw != nil {
		t.Error("draw still active after DrawEnd")
	}
	if n := len(ofType(ev, EventDrawEnded)); n != 0 {
		t.Errorf("draw_ended at the meter's end, before the attribution: %d", n)
	}
	c := planConfig()
	ev = e.Tick(last.Add(c.Draw.BalanceSettle + c.Draw.MaxSweepGap + time.Second))
	end := ofType(ev, EventDrawEnded)
	if len(end) != 1 {
		t.Fatalf("draw_ended %d after the attribution's wait, want 1", len(end))
	}
	d := end[0].Ended
	if d.DrawID != a.DrawID || !d.StartedAt.Equal(start) || !d.EndedAt.Equal(last) || d.Litres != 5 || d.VolumeKind != "metered" ||
		d.Kind != meter.KindTap || d.DurationS != 40 || d.From.Seq != 2 || d.To.Seq != 10 || len(d.Tanks) != 2 {
		t.Errorf("draw_ended %+v", d)
	}
	if d.HeatBalanceReliable || len(d.Reasons) == 0 {
		t.Errorf("no sweeps, yet reliable %v %v", d.HeatBalanceReliable, d.Reasons)
	}
	if !end[0].At.After(last) {
		t.Errorf("draw_ended at %v, before its last litre", end[0].At)
	}
}

// Recent keeps the last RecentDraws ended draws, oldest first.
func TestRecentIsARing(t *testing.T) {
	cfg := planConfig()
	cfg.RecentDraws = 2
	e := newEngine(t, cfg)
	m := &meterSim{since: t0.Add(-time.Hour)}
	e.Meter(m.msg(t0, tanktemp.TriggerSweep, tanktemp.LevelOpen))
	var ids []string
	at := t0
	for i := 0; i < 3; i++ {
		at = at.Add(time.Hour)
		for _, x := range m.draw(e, at, 2+i, 10*time.Second) {
			if x.Type == EventDrawStarted {
				ids = append(ids, x.Draw.DrawID)
			}
		}
		e.Tick(at.Add(30 * time.Minute))
	}
	got := e.Recent()
	if len(got) != 2 || got[0].DrawID != ids[1] || got[1].DrawID != ids[2] || got[0].Litres != 3 || got[1].Litres != 4 {
		t.Errorf("recent %+v, want draws %v (3 L, 4 L)", got, ids[1:])
	}
	got[0].DrawID = "changed"
	if e.Recent()[0].DrawID == "changed" {
		t.Error("Recent shares its buffer with the caller")
	}
}

// Every call's events come in event-time order.
func TestEventsSorted(t *testing.T) {
	e := newEngine(t, planConfig())
	m := &meterSim{since: t0.Add(-time.Hour)}
	e.Meter(m.msg(t0, tanktemp.TriggerSweep, tanktemp.LevelOpen))
	var all [][]Event
	for i := 0; i < 40; i++ {
		at := t0.Add(time.Duration(i) * 30 * time.Second)
		for _, id := range []string{left, right} {
			ev, err := e.TankState(uniform(id, at, 50-float64(i)/10))
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, ev)
		}
		if i == 4 {
			all = append(all, m.draw(e, at.Add(time.Second), 30, 10*time.Second))
		}
	}
	all = append(all, e.Tick(t0.Add(time.Hour)))
	n := 0
	for _, ev := range all {
		n += len(ev)
		if !sort.SliceIsSorted(ev, func(i, j int) bool { return ev[i].At.Before(ev[j].At) }) {
			t.Errorf("unsorted: %+v", ev)
		}
	}
	if n == 0 {
		t.Fatal("no events")
	}
}

// A draw's end reaches the tanks from the sweeps alone: with no meter
// message after the last litre, the meter is ticked to each sweep's time,
// so the draw ends DrawEnd after its last litre and the tanks stop drawing
// DrawSettle after that.
func TestDrawEndReachesTheTanksOnSweeps(t *testing.T) {
	c := planConfig()
	e := newEngine(t, c)
	m := &meterSim{since: t0.Add(-time.Hour)}
	e.Meter(m.msg(t0, tanktemp.TriggerSweep, tanktemp.LevelOpen))
	start := t0.Add(5 * time.Minute)
	last := start.Add(40 * time.Second)
	fed := false
	for at := t0; at.Before(t0.Add(20 * time.Minute)); at = at.Add(30 * time.Second) {
		if !fed && at.After(start) {
			m.draw(e, start, 5, 10*time.Second)
			fed = true
		}
		for _, id := range []string{left, right} {
			if _, err := e.TankState(uniform(id, at, 55-at.Sub(t0).Hours())); err != nil {
				t.Fatal(err)
			}
		}
		s := e.State()
		switch {
		case at.After(start) && at.Before(last):
			if s.ActiveDraw == nil || s.Tanks[0].State != "drawing" || s.Tanks[1].State != "drawing" {
				t.Errorf("+%v during the draw: active %v, tanks %s %s", at.Sub(t0), s.ActiveDraw, s.Tanks[0].State, s.Tanks[1].State)
			}
		case at.After(last.Add(c.Meter.DrawEnd + c.Thermal.DrawSettle)):
			if s.ActiveDraw != nil || s.Tanks[0].State == "drawing" || s.Tanks[1].State == "drawing" {
				t.Errorf("+%v after the draw: active %v, tanks %s %s", at.Sub(t0), s.ActiveDraw, s.Tanks[0].State, s.Tanks[1].State)
			}
		}
	}
}
