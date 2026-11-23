package engine

import (
	"math"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/vessel"
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

func hb(at time.Time, hw, ch boiler.Switch) boiler.System {
	return boiler.System{Shape: boiler.ShapeStatus, Event: boiler.SystemHeartbeat, Timestamp: at, HW: hw, CH: ch, Ready: bp(true)}
}

func lastWill(at time.Time) boiler.System {
	return boiler.System{Shape: boiler.ShapeSystem, Event: boiler.SystemShutdown, Reason: "MQTT_DISCONNECT", Source: "last_will", Timestamp: at}
}

// A last will dated after the last applied programmer input makes HW and
// CH unknown; one dated before it (a retained will from the last connect)
// is ignored. A heartbeat after the will makes them known again.
func TestBoilerLastWill(t *testing.T) {
	e := newEngine(t, planConfig())
	e.BoilerSystem(hb(t0, boiler.On, boiler.Off))
	e.BoilerSystem(lastWill(t0.Add(-time.Minute)))
	if b := e.State().Boiler; boolStr(b.HW) != "on" || boolStr(b.CH) != "off" {
		t.Fatalf("an older will changed hw %s ch %s", boolStr(b.HW), boolStr(b.CH))
	}
	e.BoilerSystem(lastWill(t0.Add(time.Minute)))
	if b := e.State().Boiler; b.HW != nil || b.CH != nil {
		t.Fatalf("after a newer will: hw %s ch %s, want unknown", boolStr(b.HW), boolStr(b.CH))
	}
	e.BoilerSystem(hb(t0.Add(30*time.Second), boiler.On, boiler.On))
	if b := e.State().Boiler; b.HW != nil {
		t.Errorf("a heartbeat older than the will applied: hw %s", boolStr(b.HW))
	}
	e.BoilerSystem(hb(t0.Add(2*time.Minute), boiler.Off, boiler.On))
	if b := e.State().Boiler; boolStr(b.HW) != "off" || boolStr(b.CH) != "on" {
		t.Errorf("after the next heartbeat: hw %s ch %s, want off on", boolStr(b.HW), boolStr(b.CH))
	}
}

// More than BoilerMissedHeartbeats × the heartbeat interval of event time
// without a heartbeat or event makes HW and CH unknown, judged on Tick and
// on any input. The interval is the heartbeat's own heartbeat_ms when it
// carries one, else BoilerHeartbeat.
func TestBoilerSilence(t *testing.T) {
	cfg := planConfig()
	limit := time.Duration(float64(cfg.BoilerHeartbeat) * cfg.BoilerMissedHeartbeats)
	e := newEngine(t, cfg)
	e.BoilerSystem(hb(t0, boiler.On, boiler.Off))
	e.Tick(t0.Add(limit))
	if b := e.State().Boiler; boolStr(b.HW) != "on" {
		t.Fatalf("at exactly the limit: hw %s, want still on", boolStr(b.HW))
	}
	e.Tick(t0.Add(limit + time.Second))
	if b := e.State().Boiler; b.HW != nil || b.CH != nil {
		t.Fatalf("past the limit: hw %s ch %s, want unknown", boolStr(b.HW), boolStr(b.CH))
	}
	// on an input, not only on Tick
	e = newEngine(t, cfg)
	e.Ambient(t0, 20) // listening from t0 (see TestBoilerSilenceGraceAtStartup)
	e.BoilerSystem(hb(t0, boiler.On, boiler.Off))
	e.Ambient(t0.Add(limit+time.Second), 20)
	if b := e.State().Boiler; b.HW != nil {
		t.Errorf("past the limit on an input: hw %s, want unknown", boolStr(b.HW))
	}
	// an event resets the clock as a heartbeat does
	e = newEngine(t, cfg)
	e.BoilerSystem(hb(t0, boiler.On, boiler.Off))
	e.BoilerEvent(boiler.Event{Timestamp: t0.Add(limit / 2), Event: boiler.EventCHOn, HW: boiler.On, CH: boiler.On})
	e.Tick(t0.Add(limit + time.Second))
	if b := e.State().Boiler; boolStr(b.HW) != "on" || boolStr(b.CH) != "on" {
		t.Errorf("an event inside the limit: hw %s ch %s, want on on", boolStr(b.HW), boolStr(b.CH))
	}
	// the heartbeat's own interval
	e = newEngine(t, cfg)
	h := hb(t0, boiler.On, boiler.Off)
	ms := int64(time.Minute / time.Millisecond)
	h.HeartbeatMS = &ms
	e.BoilerSystem(h)
	e.Tick(t0.Add(time.Duration(float64(time.Minute)*cfg.BoilerMissedHeartbeats) + time.Second))
	if b := e.State().Boiler; b.HW != nil {
		t.Errorf("past 2.5 × the heartbeat's 1 min: hw %s, want unknown", boolStr(b.HW))
	}
}

// The HW going unknown on silence reaches the draw attribution: a draw
// inside it is hw_unknown, so its shares are unknown.
func TestBoilerSilenceReachesDraws(t *testing.T) {
	cfg := planConfig()
	e := newEngine(t, cfg)
	e.BoilerSystem(hb(t0, boiler.Off, boiler.Off))
	m := &meterSim{since: t0.Add(-time.Hour)}
	e.Meter(m.msg(t0, tanktemp.TriggerSweep, tanktemp.LevelOpen))
	start := t0.Add(time.Hour)
	var ev []Event
	fed := false
	for at := start.Add(-time.Minute); at.Before(start.Add(15 * time.Minute)); at = at.Add(30 * time.Second) {
		if !fed && at.After(start) {
			ev = append(ev, m.draw(e, start, 5, 10*time.Second)...)
			fed = true
		}
		for _, id := range []string{left, right} {
			x, err := e.TankState(uniform(id, at, 55))
			if err != nil {
				t.Fatal(err)
			}
			ev = append(ev, x...)
		}
	}
	ev = append(ev, e.Tick(start.Add(time.Hour))...)
	b := ofType(ev, EventDrawBalanced)
	if len(b) != 1 || !slices.Contains(b[0].Balanced.Reasons, draw.ReasonHWUnknown) {
		t.Fatalf("draw_balanced %+v, want one with hw_unknown", b)
	}
}

// A programmer input older than the last applied one is ignored: a
// retained message delivered on a reconnect after a newer heartbeat can't
// roll HW and CH back.
func TestBoilerOlderInputIgnored(t *testing.T) {
	e := newEngine(t, planConfig())
	e.BoilerSystem(hb(t0, boiler.On, boiler.On))
	e.BoilerEvent(boiler.Event{Timestamp: t0.Add(-time.Minute), Event: boiler.EventHWOff, HW: boiler.Off, CH: boiler.Off})
	e.BoilerSystem(hb(t0.Add(-30*time.Second), boiler.Off, boiler.Off))
	if b := e.State().Boiler; boolStr(b.HW) != "on" || boolStr(b.CH) != "on" {
		t.Errorf("older inputs applied: hw %s ch %s, want on on", boolStr(b.HW), boolStr(b.CH))
	}
	e.BoilerEvent(boiler.Event{Timestamp: t0, Event: boiler.EventHWOff, HW: boiler.Off, CH: boiler.On})
	if b := e.State().Boiler; boolStr(b.HW) != "off" {
		t.Errorf("an input at the same time as the last: hw %s, want off", boolStr(b.HW))
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

// lowFeed is two tanks' sweeps at at: the left hot at the top, the right
// at right's temperatures.
func lowFeed(t *testing.T, e *Engine, at time.Time, right5 [5]float64) {
	t.Helper()
	if _, err := e.TankState(sweep(left, at, f(15), f(20), f(45), f(50), f(55))); err != nil {
		t.Fatal(err)
	}
	r := right5
	if _, err := e.TankState(sweep(right, at, &r[0], &r[1], &r[2], &r[3], &r[4])); err != nil {
		t.Fatal(err)
	}
}

var (
	rightLow  = [5]float64{15, 15, 20, 38, 41}
	rightMid  = [5]float64{15, 20, 45, 50, 52}
	rightHigh = [5]float64{15, 30, 55, 60, 62}
)

// systemUsable is what the engine reports for the system with the right
// tank at right5.
func systemUsable(t *testing.T, right5 [5]float64) float64 {
	e := newEngine(t, planConfig())
	lowFeed(t, e, t0, right5)
	return *e.State().UsableL40Low
}

// The system's usable litres come from a draw through the merged outlet at
// OutletSplit (estimate.Merged), not the tanks summed; stored_kwh still
// sums. With the right tank nearly spent and taking most of the flow, the
// system has far less than the left tank alone holds.
func TestSystemUsableThroughTheMergedOutlet(t *testing.T) {
	cfg := planConfig()
	e := newEngine(t, cfg)
	lowFeed(t, e, t0, rightLow)
	s := e.State()
	v, err := vessel.New(cfg.Vessel)
	if err != nil {
		t.Fatal(err)
	}
	est, err := estimate.New(v, cfg.Estimate)
	if err != nil {
		t.Fatal(err)
	}
	var ps []*profile.Profile
	for _, id := range []string{left, right} {
		sw, _ := e.LastSweep(id)
		ps = append(ps, sw.Profile)
	}
	want, err := est.Merged(ps, cfg.OutletSplit)
	if err != nil {
		t.Fatal(err)
	}
	if s.UsableL40 == nil || *s.UsableL40 != want.UsableL40 || s.UsableL40Low == nil || *s.UsableL40Low != want.UsableL40Low {
		t.Errorf("system usable %s / %s, want the merged outlet's %.2f / %.2f", fm(s.UsableL40, "%.2f"), fm(s.UsableL40Low, "%.2f"), want.UsableL40, want.UsableL40Low)
	}
	l := *s.Tanks[0].UsableL40
	if !(*s.UsableL40 < l) {
		t.Errorf("system usable %.1f L, want less than the left tank alone (%.1f L)", *s.UsableL40, l)
	}
	if sum := *s.Tanks[0].StoredKWh + *s.Tanks[1].StoredKWh; s.StoredKWh == nil || *s.StoredKWh != sum {
		t.Errorf("stored %s, want the tanks summed %.3f", fm(s.StoredKWh, "%.3f"), sum)
	}
	t.Logf("left usable_l40 %.1f L, right %.1f L; system %.1f L (first litre %.1f °C)", l, *s.Tanks[1].UsableL40, *s.UsableL40, want.FirstC)
}

// low_hot_water sets when the system's usable_l40_low falls below
// LowHotWaterL (not at it), and clears only when it rises above
// LowHotWaterL + LowHotWaterMarginL (not at it), holding its value in
// between.
func TestLowHotWaterHysteresis(t *testing.T) {
	uLow, uMid, uHigh := systemUsable(t, rightLow), systemUsable(t, rightMid), systemUsable(t, rightHigh)
	t.Logf("system usable_l40_low: %.2f, %.2f, %.2f L", uLow, uMid, uHigh)
	if !(uLow < uMid && uMid < uHigh) {
		t.Fatalf("fixture: %.2f, %.2f, %.2f should rise", uLow, uMid, uHigh)
	}
	const eps = 1e-6
	run := func(l, margin float64, rights ...[5]float64) []string {
		cfg := planConfig()
		cfg.LowHotWaterL, cfg.LowHotWaterMarginL = l, margin
		e := newEngine(t, cfg)
		var out []string
		for i, r := range rights {
			lowFeed(t, e, t0.Add(time.Duration(i)*30*time.Second), r)
			out = append(out, boolStr(e.State().LowHotWater))
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		l, m   float64
		rights [][5]float64
		want   []string
	}{
		{"sets just below", uLow + eps, 0, [][5]float64{rightLow}, []string{"on"}},
		{"not at the threshold", uLow, 0, [][5]float64{rightLow}, []string{"off"}},
		{"holds inside the margin", uLow + eps, uMid - uLow, [][5]float64{rightLow, rightMid}, []string{"on", "on"}},
		{"holds at the clear level", uLow + eps, uHigh - uLow - eps, [][5]float64{rightLow, rightHigh}, []string{"on", "on"}},
		{"clears just above", uLow + eps, uHigh - uLow - 3*eps, [][5]float64{rightLow, rightHigh}, []string{"on", "off"}},
		{"off stays off inside the margin", uLow - eps, uHigh, [][5]float64{rightMid, rightLow}, []string{"off", "off"}},
		{"sets again after clearing", uMid + eps, 0, [][5]float64{rightLow, rightHigh, rightLow}, []string{"on", "off", "on"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.l, tc.m, tc.rights...); !slices.Equal(got, tc.want) {
				t.Errorf("low %v, want %v", got, tc.want)
			}
		})
	}
}

// Unknown while a tank's estimate is (stale), and the held value comes
// back when it is known again: a value inside the margin after the gap
// keeps what was set before it.
func TestLowHotWaterUnknownHolds(t *testing.T) {
	uLow, uMid := systemUsable(t, rightLow), systemUsable(t, rightMid)
	cfg := planConfig()
	cfg.LowHotWaterL, cfg.LowHotWaterMarginL = uLow+1e-6, uMid-uLow+1
	e := newEngine(t, cfg)
	lowFeed(t, e, t0, rightLow)
	if got := boolStr(e.State().LowHotWater); got != "on" {
		t.Fatalf("low %s, want on", got)
	}
	e.Tick(t0.Add(cfg.Thermal.StaleAfter + time.Second))
	if s := e.State(); s.LowHotWater != nil || s.UsableL40Low != nil {
		t.Fatalf("stale: low %s usable %s, want unknown", boolStr(s.LowHotWater), fm(s.UsableL40Low, "%.1f"))
	}
	lowFeed(t, e, t0.Add(cfg.Thermal.StaleAfter+time.Minute), rightMid)
	if got := boolStr(e.State().LowHotWater); got != "on" {
		t.Errorf("known again inside the margin: low %s, want the held on", got)
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

// A running draw is in the state with draw_started's id and start. It
// stays there after the meter ends it (DrawEnd after its last litre) until
// draw_ended, and leaves in the same input that gives draw_ended: once
// every tank's share sweep is in, or (here, with no sweeps) the wait for
// them is over. draw_ended carries the meter's draw and no balance;
// draw_balanced follows with the attribution, the same draw_id and the
// same ended_at.
func TestDrawEndedThenBalanced(t *testing.T) {
	c := planConfig()
	e := newEngine(t, c)
	e.BoilerSystem(hb(t0, boiler.Off, boiler.Off))
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
	ev = e.Tick(last.Add(c.Meter.DrawEnd))
	if n := len(ofType(ev, EventDrawEnded)); n != 0 {
		t.Errorf("draw_ended at the meter's end, before the share sweeps' wait: %d", n)
	}
	if a := e.State().ActiveDraw; a == nil || a.DrawID != st[0].Draw.DrawID || a.LitresSoFar != 5 {
		t.Fatalf("active %+v after the meter's end, want it held until draw_ended", a)
	}
	deadline := last.Add(c.Draw.ShareSettle + c.Draw.MaxSweepGap)
	if ev = e.Tick(deadline); len(ofType(ev, EventDrawEnded)) != 0 || e.State().ActiveDraw == nil {
		t.Fatalf("draw_ended at the deadline itself")
	}
	ev = e.Tick(deadline.Add(time.Second))
	end := ofType(ev, EventDrawEnded)
	if len(end) != 1 || e.State().ActiveDraw != nil {
		t.Fatalf("draw_ended %d, active %+v past the share sweeps' wait, want one and the draw gone", len(end), e.State().ActiveDraw)
	}
	d := end[0].Ended
	if d.DrawID != a.DrawID || !d.StartedAt.Equal(start) || !d.EndedAt.Equal(last) || d.Litres != 5 || d.VolumeKind != "metered" ||
		d.Kind != meter.KindTap || d.DurationS != 40 || d.From.Seq != 2 || d.To.Seq != 10 || d.FlowLPerMin == nil {
		t.Errorf("draw_ended %+v", d)
	}
	if !end[0].At.Equal(deadline.Add(time.Second)) {
		t.Errorf("draw_ended at %v, want %v", end[0].At, deadline.Add(time.Second))
	}
	if n := len(ofType(ev, EventDrawBalanced)); n != 0 {
		t.Errorf("draw_balanced with draw_ended: %d", n)
	}
	ev = e.Tick(last.Add(c.Draw.BalanceSettle + c.Draw.MaxSweepGap + time.Second))
	bal := ofType(ev, EventDrawBalanced)
	if len(bal) != 1 {
		t.Fatalf("draw_balanced %d after the attribution's wait, want 1", len(bal))
	}
	b := bal[0].Balanced
	if b.DrawID != d.DrawID || !b.EndedAt.Equal(d.EndedAt) || len(b.Tanks) != 2 {
		t.Errorf("draw_balanced %+v, want draw %s ended at %v", b, d.DrawID, d.EndedAt)
	}
	if b.HeatBalanceReliable || len(b.Reasons) == 0 {
		t.Errorf("no sweeps, yet reliable %v %v", b.HeatBalanceReliable, b.Reasons)
	}
	if e.Pending() {
		t.Error("still pending")
	}
}

// The next draw starting gives the previous draw's draw_ended in the same
// input, so active_draw moves straight from one to the next.
func TestNextDrawEndsTheHeldDraw(t *testing.T) {
	c := planConfig()
	e := newEngine(t, c)
	m := &meterSim{since: t0.Add(-time.Hour)}
	e.Meter(m.msg(t0, tanktemp.TriggerSweep, tanktemp.LevelOpen))
	start := t0.Add(time.Minute)
	m.draw(e, start, 3, 10*time.Second)
	last := start.Add(20 * time.Second)
	e.Tick(last.Add(c.Meter.DrawEnd + time.Second))
	held := e.State().ActiveDraw
	if held == nil {
		t.Fatal("no held draw")
	}
	ev := m.draw(e, last.Add(c.Meter.DrawEnd+20*time.Second), 3, 10*time.Second)
	end, st := ofType(ev, EventDrawEnded), ofType(ev, EventDrawStarted)
	if len(end) != 1 || end[0].Ended.DrawID != held.DrawID || len(st) != 1 {
		t.Fatalf("draw_ended %+v, draw_started %+v: want the held draw ended as the next starts", end, st)
	}
	if a := e.State().ActiveDraw; a == nil || a.DrawID != st[0].Draw.DrawID {
		t.Errorf("active %+v, want the new draw", a)
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
	for _, r := range got {
		if r.Balanced == nil || r.Balanced.DrawID != r.DrawID {
			t.Errorf("recent %s: balance %+v, want its draw_balanced", r.DrawID, r.Balanced)
		}
	}
	got[0].DrawID = "changed"
	got[0].Balanced.DrawID = "changed"
	if r := e.Recent()[0]; r.DrawID == "changed" || r.Balanced.DrawID == "changed" {
		t.Error("Recent shares its buffer with the caller")
	}
	// A draw ended but not yet balanced is in Recent without its balance.
	at = at.Add(time.Hour)
	m.draw(e, at, 2, 10*time.Second)
	c := planConfig()
	e.Tick(at.Add(10*time.Second + c.Draw.ShareSettle + c.Draw.MaxSweepGap + time.Second))
	if got := e.Recent(); len(got) != 2 || got[1].Litres != 2 || got[1].Balanced != nil {
		t.Errorf("recent %+v, want the new draw last, unbalanced", got)
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

// A just-started engine hasn't been listening long enough to call the
// programmer silent: retained messages replay old programmer inputs (the
// fixture's retained HW event is 59 min older than its retained heartbeat).
// Silence is judged from the later of the last input and the engine's
// first event time.
func TestBoilerSilenceGraceAtStartup(t *testing.T) {
	cfg := planConfig()
	limit := time.Duration(float64(cfg.BoilerHeartbeat) * cfg.BoilerMissedHeartbeats)
	e := newEngine(t, cfg)
	e.Ambient(t0, 20) // the first input: event time starts here
	e.BoilerEvent(boiler.Event{Timestamp: t0.Add(-59 * time.Minute), Event: boiler.EventHWOn, HW: boiler.On, CH: boiler.Off})
	if b := e.State().Boiler; boolStr(b.HW) != "on" {
		t.Fatalf("a retained event at startup: hw %s, want on", boolStr(b.HW))
	}
	e.Tick(t0.Add(limit))
	if b := e.State().Boiler; boolStr(b.HW) != "on" {
		t.Fatalf("within the grace: hw %s, want on", boolStr(b.HW))
	}
	e.Tick(t0.Add(limit + time.Second))
	if b := e.State().Boiler; b.HW != nil {
		t.Errorf("silent for the limit after startup: hw %s, want unknown", boolStr(b.HW))
	}
	// The retained event may arrive first (as in the fixtures): a
	// programmer input's own time is old by nature, so the grace starts at
	// the first other input.
	e = newEngine(t, cfg)
	e.BoilerEvent(boiler.Event{Timestamp: t0.Add(-59 * time.Minute), Event: boiler.EventHWOn, HW: boiler.On, CH: boiler.Off})
	e.Ambient(t0, 20)
	if b := e.State().Boiler; boolStr(b.HW) != "on" {
		t.Errorf("a retained event, then the first sweep: hw %s, want on", boolStr(b.HW))
	}
}

// The state's meter time is the latest synced message's: an unsynced one
// (tanktemp booted without NTP) still counts, but its time can't be
// trusted, so it doesn't move meter.timestamp.
func TestMeterTimeOnlyFromSyncedMessages(t *testing.T) {
	e := newEngine(t, planConfig())
	m := &meterSim{since: t0}
	e.Meter(m.msg(t0, tanktemp.TriggerSweep, tanktemp.LevelOpen))
	u := m.msg(t0.Add(-time.Hour), tanktemp.TriggerSweep, tanktemp.LevelOpen)
	u.ClockSynced = false
	m.count = 3
	u.Count = 3
	e.Meter(u)
	s := e.State().Meter
	if s.At == nil || !s.At.Equal(t0) {
		t.Errorf("meter time %v, want the synced %v", s.At, t0)
	}
	if s.Count == nil || *s.Count != 3 {
		t.Errorf("count %v, want the unsynced message's 3", s.Count)
	}
}

// With only unsynced meter messages, the meter time is unknown, not zero.
func TestMeterTimeUnknownWhenNeverSynced(t *testing.T) {
	e := newEngine(t, planConfig())
	m := &meterSim{since: t0}
	u := m.msg(t0, tanktemp.TriggerSweep, tanktemp.LevelOpen)
	u.ClockSynced = false
	e.Meter(u)
	if s := e.State().Meter; s.At != nil || s.Count == nil {
		t.Errorf("meter time %v count %v, want an unknown time and a count", s.At, s.Count)
	}
}

// When the next draw starts before the last one's draw_ended is out (it
// waits for the tanks' sweeps after the meter's end), active_draw goes
// from one draw straight to the next, never null. It still changes only
// in an input that also gives the last one's draw_ended, so a consumer
// reconciling from State never loses a draw unended.
func TestNextDrawReplacesActiveDrawWithItsPredecessorsEnd(t *testing.T) {
	e := newEngine(t, planConfig())
	m := &meterSim{since: t0}
	var msgs []tanktemp.Meter
	for i := 0; i < 5; i++ { // draw A: 5 litres, 10 s apart
		msgs = append(msgs, m.litre(t0.Add(time.Duration(i)*10*time.Second), 2*time.Second)...)
	}
	b := t0.Add(40*time.Second + 90*time.Second) // B 90 s after A's last litre: A ended, not yet given
	for i := 0; i < 3; i++ {
		msgs = append(msgs, m.litre(b.Add(time.Duration(i)*10*time.Second), 2*time.Second)...)
	}
	var prev string
	sawA, sawB := false, false
	for _, x := range msgs {
		ev := e.Meter(x)
		cur := ""
		if d := e.State().ActiveDraw; d != nil {
			cur = d.DrawID
		}
		if prev != "" && cur != prev {
			ended := false
			for _, v := range ev {
				ended = ended || (v.Type == EventDrawEnded && v.Ended.DrawID == prev)
			}
			if !ended {
				t.Errorf("active_draw went %q → %q without %q's draw_ended in that input (%v)", prev, cur, prev, ev)
			}
			if cur == "" {
				t.Errorf("active_draw went null between the draws")
			}
			sawB = cur != ""
		}
		if cur != "" && prev == "" {
			sawA = true
		}
		prev = cur
	}
	if !sawA || !sawB {
		t.Fatalf("saw A %v, B replacing it %v: the scenario didn't happen", sawA, sawB)
	}
}
