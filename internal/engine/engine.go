package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/tank"
	"github.com/sweeney/washhouse/internal/thermal"
	"github.com/sweeney/washhouse/internal/vessel"
	"github.com/sweeney/washhouse/internal/wire/boiler"
	"github.com/sweeney/washhouse/internal/wire/tanktemp"
)

// Engine holds the system's in-memory state. It is not safe for concurrent
// use.
type Engine struct {
	cfg     Config
	now     time.Time // event time: the latest any input carried
	tanks   map[string]*tankSlot
	order   []string
	meter   *meter.Consumer
	thermal *thermal.Tracker
	attr    *draw.Attributor

	hw, ch boiler.Switch

	ambOK bool
	ambAt time.Time
	ambC  float64

	meterSeen     bool
	meterAt       time.Time
	meterCount    uint64
	countingSince time.Time

	tanktemp TanktempStatus

	recent []DrawEnded // a ring of the last RecentDraws, oldest first
}

// tankSlot is one cylinder's model and its latest sweep.
type tankSlot struct {
	model *tank.Tank
	seen  bool       // a sweep was taken
	last  tank.Sweep // the latest sweep taken
	ok    bool       // the latest sweep was estimated
}

// New validates cfg and returns an engine with nothing known.
func New(cfg Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	v, err := vessel.New(cfg.Vessel)
	if err != nil {
		return nil, err
	}
	est, err := estimate.New(v, cfg.Estimate)
	if err != nil {
		return nil, err
	}
	e := &Engine{cfg: cfg, tanks: map[string]*tankSlot{}}
	for _, tc := range cfg.Tanks {
		m, err := tank.New(tc, est)
		if err != nil {
			return nil, err
		}
		e.tanks[tc.DeviceID] = &tankSlot{model: m}
		e.order = append(e.order, tc.DeviceID)
	}
	if e.meter, err = meter.New(cfg.Meter); err != nil {
		return nil, err
	}
	if e.thermal, err = thermal.New(cfg.Thermal, est); err != nil {
		return nil, err
	}
	if e.attr, err = draw.New(cfg.Draw); err != nil {
		return nil, err
	}
	return e, nil
}

// see moves event time forward to t.
func (e *Engine) see(t time.Time) {
	if t.After(e.now) {
		e.now = t
	}
}

// sorted orders ev by event time, keeping the order of equal times.
func sorted(ev []Event) []Event {
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].At.Before(ev[j].At) })
	return ev
}

// TankState takes a tanktemp sweep. A sweep with an unsynced clock, or no
// later than that tank's last, is skipped. A sweep from a tank not
// configured, or one the tank's model refuses (fewer than two usable
// probes, say), is an error; the latter leaves the tank's estimate
// unknown.
func (e *Engine) TankState(st tanktemp.State) ([]Event, error) {
	slot := e.tanks[st.DeviceID]
	if slot == nil {
		return nil, fmt.Errorf("engine: state from %q, which isn't configured", st.DeviceID)
	}
	if !st.ClockSynced || (slot.seen && !st.Timestamp.After(slot.last.At)) {
		return nil, nil
	}
	e.see(st.Timestamp)
	// A draw that ended before this sweep is known before it is judged.
	ev := e.meterEvents(nil, e.meter.Tick(st.Timestamp))
	sw, err := slot.model.Sweep(st)
	slot.seen = true
	if err != nil {
		slot.ok = false
		slot.last = tank.Sweep{DeviceID: st.DeviceID, At: st.Timestamp, ClockSynced: st.ClockSynced,
			ProbesTotal: len(st.Sensors), ProbesOK: usable(st, e.trust(st.DeviceID))}
		return sorted(ev), err
	}
	slot.ok, slot.last = true, sw
	tev, err := e.thermal.Sweep(sw)
	if err != nil {
		return sorted(ev), err // can't happen: the tanks are the same, by Validate
	}
	ev = e.thermalEvents(ev, tev)
	ev = e.attributions(ev, e.attr.Sweep(sw))
	return sorted(ev), nil
}

// usable counts the readings in st the model could use.
func usable(st tanktemp.State, trustSuspect bool) int {
	n := 0
	for _, s := range st.Sensors {
		if s.TempC != nil && (s.Suspect == nil || trustSuspect) {
			n++
		}
	}
	return n
}

func (e *Engine) trust(id string) bool {
	for _, tc := range e.cfg.Tanks {
		if tc.DeviceID == id {
			return tc.TrustSuspect
		}
	}
	return false
}

// Meter takes a tanktemp meter message.
func (e *Engine) Meter(m tanktemp.Meter) []Event {
	ev := e.meter.Feed(m)
	if h := e.meter.Health(); m.DeviceID == e.cfg.Meter.DeviceID && h.Run.Seq == m.Seq && h.Run.CountingSince.Equal(m.CountingSince) {
		// taken: the consumer's latest message is this one, not one it
		// refused, or a duplicate of an earlier one
		e.meterSeen, e.meterAt, e.meterCount, e.countingSince = true, m.Timestamp, m.Count, m.CountingSince
		if m.ClockSynced {
			e.see(m.Timestamp)
		}
	}
	out := e.meterEvents(nil, ev)
	return sorted(e.attributions(out, e.attr.Tick(e.now)))
}

// meterEvents passes the meter consumer's events on: draws to the tracker
// and the attributor, and out as the engine's events.
func (e *Engine) meterEvents(out []Event, ev []meter.Event) []Event {
	for _, x := range ev {
		e.see(x.At)
		switch x.Type {
		case meter.EventDrawStarted, meter.EventDrawUpdated:
			out = append(out, Event{Type: EventType(x.Type), At: x.At, Draw: activeOf(*x.Draw)})
			if x.Type == meter.EventDrawStarted {
				out = e.thermalEvents(out, e.thermal.MeteredDraw(x.At, true))
			}
			out = e.attributions(out, e.attr.Meter(x))
		case meter.EventDrawEnded:
			out = e.thermalEvents(out, e.thermal.MeteredDraw(x.At, false))
			out = e.attributions(out, e.attr.Meter(x))
		case meter.EventContinuousFlowStarted, meter.EventContinuousFlowEnded:
			f := flowOf(*x.Flow)
			out = append(out, Event{Type: EventType(x.Type), At: x.At, Flow: &f})
		}
	}
	return out
}

func activeOf(d meter.Draw) *ActiveDraw {
	return &ActiveDraw{DrawID: d.ID, StartedAt: d.Start, LastLitreAt: d.End, LitresSoFar: d.Litres,
		FlowLPerMin: d.FlowLPerMin, Kind: d.Kind}
}

func flowOf(f meter.ContinuousFlow) Flow {
	return Flow{Since: f.Since, Last: f.Last, Litres: f.Litres, FlowLPerMin: f.FlowLPerMin}
}

func (e *Engine) thermalEvents(out []Event, ev []thermal.Event) []Event {
	for _, x := range ev {
		te := &TankEvent{DeviceID: x.DeviceID, State: x.State, Prev: x.Prev, Source: sourceOf(x.Source), PowerKW: x.PowerKW}
		out = append(out, Event{Type: EventType(x.Type), At: x.At, Tank: te})
	}
	return out
}

func sourceOf(s thermal.Source) *thermal.Source {
	if s == thermal.SourceNone {
		return nil
	}
	return &s
}

// attributions turns completed attributions into draw_ended, at the event
// time that completed them, and keeps them in the ring.
func (e *Engine) attributions(out []Event, as []draw.Attribution) []Event {
	for _, a := range as {
		d := endedOf(a)
		if len(e.recent) == e.cfg.RecentDraws {
			e.recent = append(e.recent[:0], e.recent[1:]...)
		}
		e.recent = append(e.recent, d)
		out = append(out, Event{Type: EventDrawEnded, At: e.now, Ended: &d})
	}
	return out
}

func endedOf(a draw.Attribution) DrawEnded {
	d := a.Draw
	out := DrawEnded{
		DrawID: d.ID, StartedAt: d.Start, EndedAt: d.End, DurationS: d.Duration.Seconds(),
		Litres: d.Litres, VolumeKind: "metered", FlowLPerMin: d.FlowLPerMin, Kind: d.Kind,
		OutletPeakC: a.OutletC, InletDropC: a.InletDropC, EnergyKWh: a.EnergyKWh,
		HeatBalanceL: a.HeatBalanceL, HeatBalanceReliable: a.Reliable, Reasons: a.Reasons,
		MaskedByReheat: a.MaskedByReheat, From: d.From, To: d.To,
	}
	for _, t := range a.Tanks {
		out.Tanks = append(out.Tanks, TankShare{DeviceID: t.DeviceID, Share: t.Share, Litres: t.Litres,
			OutletPeakC: t.OutletPeakC, InletDropC: t.InletDropC})
	}
	return out
}

// Status takes a tanktemp status message. The last will says only that
// tanktemp is offline.
func (e *Engine) Status(s tanktemp.Status) {
	on := s.Online
	st := TanktempStatus{Online: &on}
	if s.Timestamp != nil {
		at := *s.Timestamp
		st.At = &at
		v := s.Version
		st.Version = &v
		e.see(at)
	}
	e.tanktemp = st
}

// BoilerEvent takes a programmer switch event: it always applies.
func (e *Engine) BoilerEvent(b boiler.Event) []Event {
	return sorted(e.programmer(b.Timestamp, b.HW, b.CH))
}

// BoilerSystem takes a boiler-sensor system message. Only a HEARTBEAT with
// ready true says what HW and CH are; anything else (RECONNECTED, the last
// will, STARTUP, a heartbeat before the GPIO baseline) changes nothing.
func (e *Engine) BoilerSystem(s boiler.System) []Event {
	if !s.IsHeartbeat() || s.Ready == nil || !*s.Ready {
		return nil
	}
	return sorted(e.programmer(s.Timestamp, s.HW, s.CH))
}

func (e *Engine) programmer(at time.Time, hw, ch boiler.Switch) []Event {
	e.see(at)
	e.hw, e.ch = hw, ch
	e.attr.HW(at, hw)
	return e.thermalEvents(nil, e.thermal.Programmer(at, hw, ch))
}

// Ambient takes a boiler-room temperature read at at. One older than the
// latest is ignored.
func (e *Engine) Ambient(at time.Time, tempC float64) []Event {
	if e.ambOK && at.Before(e.ambAt) {
		return nil
	}
	e.see(at)
	e.ambOK, e.ambAt, e.ambC = true, at, tempC
	e.thermal.Ambient(at, tempC)
	return nil
}

// Tick moves event time to t (never back) with no input, and returns what
// that ends: draws, continuous flows, attributions waiting on sweeps, a
// tank going stale.
func (e *Engine) Tick(t time.Time) []Event {
	e.see(t)
	ev := e.meterEvents(nil, e.meter.Tick(e.now))
	ev = e.attributions(ev, e.attr.Tick(e.now))
	ev = e.thermalEvents(ev, e.thermal.Tick(e.now))
	return sorted(ev)
}

// Settle is how long after the last input a Tick must come for every draw
// to have ended and been attributed: the meter's DrawEnd, then the
// attribution's longest wait.
func (e *Engine) Settle() time.Duration {
	return e.cfg.Meter.DrawEnd + e.cfg.Draw.BalanceSettle + e.cfg.Draw.MaxSweepGap + time.Second
}

// Pending says whether a draw is running or waiting for its attribution.
func (e *Engine) Pending() bool {
	_, running := e.meter.Draw()
	return running || e.attr.Pending() > 0
}

// State reports the system as of the latest input.
func (e *Engine) State() State {
	s := State{At: e.now, Tanktemp: e.tanktemp}
	total := [3]float64{}
	all := len(e.order) > 0
	for _, id := range e.order {
		ts := e.tankState(id)
		if ts.StoredKWh == nil {
			all = false
		} else {
			total[0] += *ts.StoredKWh
			total[1] += *ts.UsableL40
			total[2] += *ts.UsableL40Low
		}
		s.Tanks = append(s.Tanks, ts)
	}
	if all {
		s.StoredKWh, s.UsableL40, s.UsableL40Low = &total[0], &total[1], &total[2]
		low := total[2] < e.cfg.LowHotWaterL
		s.LowHotWater = &low
	}
	s.Boiler = Boiler{HW: switchOf(e.hw), CH: switchOf(e.ch)}
	if e.ambOK && e.now.Sub(e.ambAt) <= e.cfg.Thermal.AmbientStale {
		c := e.ambC
		s.AmbientC = &c
	}
	if d, ok := e.meter.Draw(); ok {
		s.ActiveDraw = activeOf(d)
	}
	if e.meterSeen {
		at, n, since, l := e.meterAt, e.meterCount, e.countingSince, e.meter.Health().Litres
		s.Meter = MeterState{At: &at, Count: &n, CountingSince: &since, Litres: &l}
		if f, ok := e.meter.Flow(); ok {
			x := flowOf(f)
			s.Meter.ContinuousFlow = &x
		}
	}
	return s
}

func (e *Engine) tankState(id string) TankState {
	slot := e.tanks[id]
	ts := TankState{DeviceID: id, State: thermal.StateUnknown}
	if st, ok := e.thermal.Tank(id); ok {
		ts.State, ts.HeatSource, ts.PowerKW = st.State, sourceOf(st.Source), st.PowerKW
		ua := st.UA.WPerK
		ts.UAWPerK = &ua
	}
	if !slot.seen {
		return ts
	}
	sw := slot.last
	at, okN, total := sw.At, sw.ProbesOK, sw.ProbesTotal
	ts.At, ts.ProbesOK, ts.ProbesTotal = &at, &okN, &total
	if !slot.ok || e.now.Sub(sw.At) > e.cfg.Thermal.StaleAfter {
		return ts
	}
	x := sw.Estimate
	ts.StoredKWh, ts.MeanC, ts.TopC = &x.KWh, &x.MeanC, &x.TopC
	ts.HotL, ts.UsableL40, ts.UsableL40Low = &x.HotL, &x.UsableL40, &x.UsableL40Low
	return ts
}

func switchOf(s boiler.Switch) *bool {
	switch s {
	case boiler.On:
		b := true
		return &b
	case boiler.Off:
		b := false
		return &b
	}
	return nil
}

// Recent returns the ended draws kept (the last RecentDraws), oldest
// first. The slice is the caller's.
func (e *Engine) Recent() []DrawEnded {
	return append([]DrawEnded(nil), e.recent...)
}

// LastSweep returns tank id's latest estimated sweep, with its probes and
// profile.
func (e *Engine) LastSweep(id string) (tank.Sweep, bool) {
	slot := e.tanks[id]
	if slot == nil || !slot.seen || !slot.ok {
		return tank.Sweep{}, false
	}
	return slot.last, true
}
