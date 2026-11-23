package engine

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/profile"
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
	started time.Time // the first non-programmer input's time: silence is judged from no earlier
	tanks   map[string]*tankSlot
	order   []string
	meter   *meter.Consumer
	thermal *thermal.Tracker
	attr    *draw.Attributor
	est     *estimate.Estimator

	// The programmer: its switches, the time of the last input applied
	// (a heartbeat, an event or a last will), the heartbeat interval, and
	// whether silence has already made the switches unknown.
	hw, ch     boiler.Switch
	boilerSeen bool
	boilerAt   time.Time
	interval   time.Duration
	silent     bool

	held []meter.Draw // ended by the meter, draw_ended not yet given
	low  *bool        // low_hot_water's held value; nil before it was judged

	ambOK bool
	ambAt time.Time
	ambC  float64

	meterSeen     bool
	meterAt       time.Time
	meterCount    uint64
	countingSince time.Time

	tanktemp TanktempStatus

	recent []DrawRecord // a ring of the last RecentDraws, oldest first
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
	e := &Engine{cfg: cfg, tanks: map[string]*tankSlot{}, est: est, interval: cfg.BoilerHeartbeat}
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
	if e.started.IsZero() {
		e.started = t
	}
	e.advance(t)
}

// advance moves event time on without starting the silence grace: a
// programmer input's own time is old when it is a retained replay.
func (e *Engine) advance(t time.Time) {
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
		return e.done(nil), nil
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
		return e.done(ev), err
	}
	slot.ok, slot.last = true, sw
	tev, err := e.thermal.Sweep(sw)
	if err != nil {
		return e.done(ev), err // can't happen: the tanks are the same, by Validate
	}
	ev = e.thermalEvents(ev, tev)
	ev = e.attributions(ev, e.attr.Sweep(sw))
	return e.done(ev), nil
}

// done finishes an input: the programmer's silence and low_hot_water are
// judged at the new event time, and the events are sorted.
func (e *Engine) done(ev []Event) []Event {
	ev = e.boilerSilence(ev)
	e.judgeLow()
	return sorted(ev)
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
		e.meterSeen, e.meterCount, e.countingSince = true, m.Count, m.CountingSince
		if m.ClockSynced {
			e.meterAt = m.Timestamp // an unsynced time can't be trusted
			e.see(m.Timestamp)
		}
	}
	out := e.meterEvents(nil, ev)
	return e.done(e.attributions(out, e.attr.Tick(e.now)))
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
			e.held = append(e.held, *x.Draw)
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

// attributions turns the attributor's results into draw_ended and
// draw_balanced, at the event time that completed them, and keeps them in
// the ring. draw_ended releases the draw the state held as active.
func (e *Engine) attributions(out []Event, rs []draw.Result) []Event {
	for _, r := range rs {
		switch r.Stage {
		case draw.StageEnded:
			d := endedOf(r.Draw)
			e.held = slices.DeleteFunc(e.held, func(h meter.Draw) bool { return h.ID == d.DrawID })
			if len(e.recent) == e.cfg.RecentDraws {
				e.recent = append(e.recent[:0], e.recent[1:]...)
			}
			e.recent = append(e.recent, DrawRecord{DrawEnded: d})
			out = append(out, Event{Type: EventDrawEnded, At: e.now, Ended: &d})
		case draw.StageBalanced:
			b := balancedOf(r.Attribution)
			for i := range e.recent {
				if e.recent[i].DrawID == b.DrawID {
					x := b
					e.recent[i].Balanced = &x
				}
			}
			out = append(out, Event{Type: EventDrawBalanced, At: e.now, Balanced: &b})
		}
	}
	return out
}

func endedOf(d meter.Draw) DrawEnded {
	return DrawEnded{
		DrawID: d.ID, StartedAt: d.Start, EndedAt: d.End, DurationS: d.Duration.Seconds(),
		Litres: d.Litres, VolumeKind: "metered", FlowLPerMin: d.FlowLPerMin, Kind: d.Kind, From: d.From, To: d.To,
	}
}

func balancedOf(a draw.Attribution) DrawBalanced {
	out := DrawBalanced{
		DrawID: a.Draw.ID, EndedAt: a.Draw.End,
		OutletPeakC: a.OutletC, InletDropC: a.InletDropC, EnergyKWh: a.EnergyKWh,
		HeatBalanceL: a.HeatBalanceL, HeatBalanceReliable: a.Reliable, Reasons: a.Reasons,
		MaskedByReheat: a.MaskedByReheat,
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
		e.advance(at) // a retained status can be old, as a programmer input
	}
	e.tanktemp = st
	e.done(nil)
}

// BoilerEvent takes a programmer switch event. It applies unless it is
// older than the last programmer input applied.
func (e *Engine) BoilerEvent(b boiler.Event) []Event {
	return e.done(e.programmer(b.Timestamp, b.HW, b.CH))
}

// BoilerSystem takes a boiler-sensor system message. A HEARTBEAT with
// ready true says what HW and CH are (boiler.System.ReadyHeartbeat), and
// sets the heartbeat interval if it carries one. A SHUTDOWN (the last
// will, or a clean shutdown) makes both unknown. Either applies only if it
// is no older than the last programmer input applied: a last will is dated
// when boiler-sensor connected, so a retained one from before the latest
// heartbeat changes nothing. Anything else (RECONNECTED, STARTUP, a
// heartbeat before the GPIO baseline) changes nothing.
func (e *Engine) BoilerSystem(s boiler.System) []Event {
	switch {
	case s.ReadyHeartbeat():
		if s.HeartbeatMS != nil && *s.HeartbeatMS > 0 && !e.older(s.Timestamp) {
			e.interval = time.Duration(*s.HeartbeatMS) * time.Millisecond
		}
		return e.done(e.programmer(s.Timestamp, s.HW, s.CH))
	case s.Event == boiler.SystemShutdown:
		return e.done(e.programmer(s.Timestamp, boiler.Unknown, boiler.Unknown))
	}
	return e.done(nil)
}

// older says whether at is before the last programmer input applied.
func (e *Engine) older(at time.Time) bool { return e.boilerSeen && at.Before(e.boilerAt) }

func (e *Engine) programmer(at time.Time, hw, ch boiler.Switch) []Event {
	if e.older(at) {
		return nil
	}
	e.advance(at)
	e.boilerSeen, e.boilerAt, e.silent = true, at, false
	return e.setProgrammer(at, hw, ch)
}

func (e *Engine) setProgrammer(at time.Time, hw, ch boiler.Switch) []Event {
	e.hw, e.ch = hw, ch
	e.attr.HW(at, hw)
	return e.thermalEvents(nil, e.thermal.Programmer(at, hw, ch))
}

// boilerSilence makes HW and CH unknown once event time is more than
// BoilerMissedHeartbeats intervals past the last programmer input (or past
// the engine's first event time, if later), dated at that limit.
func (e *Engine) boilerSilence(out []Event) []Event {
	if !e.boilerSeen || e.silent {
		return out
	}
	// Retained messages replay old programmer inputs at startup, so the
	// clock runs from the later of the last input and the first other
	// input's time (tank sweeps, the room, the meter carry current times).
	from := e.boilerAt
	if e.started.After(from) {
		from = e.started
	}
	limit := from.Add(time.Duration(float64(e.interval) * e.cfg.BoilerMissedHeartbeats))
	if !e.now.After(limit) {
		return out
	}
	e.silent = true
	if e.hw == boiler.Unknown && e.ch == boiler.Unknown {
		return out
	}
	return append(out, e.setProgrammer(limit, boiler.Unknown, boiler.Unknown)...)
}

// Ambient takes a boiler-room temperature read at at. One older than the
// latest is ignored.
func (e *Engine) Ambient(at time.Time, tempC float64) []Event {
	if e.ambOK && at.Before(e.ambAt) {
		return e.done(nil)
	}
	e.see(at)
	e.ambOK, e.ambAt, e.ambC = true, at, tempC
	e.thermal.Ambient(at, tempC)
	return e.done(nil)
}

// Tick moves event time to t (never back) with no input, and returns what
// that ends: draws, continuous flows, attributions waiting on sweeps, a
// tank going stale.
func (e *Engine) Tick(t time.Time) []Event {
	e.advance(t) // time passing isn't an input: it doesn't start the grace
	ev := e.meterEvents(nil, e.meter.Tick(e.now))
	ev = e.attributions(ev, e.attr.Tick(e.now))
	ev = e.thermalEvents(ev, e.thermal.Tick(e.now))
	return e.done(ev)
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
	return running || len(e.held) > 0 || e.attr.Pending() > 0
}

// State reports the system as of the latest input.
func (e *Engine) State() State {
	s := State{At: e.now, Tanktemp: e.tanktemp}
	for _, id := range e.order {
		s.Tanks = append(s.Tanks, e.tankState(id))
	}
	if sys, ok := e.system(); ok {
		s.StoredKWh, s.UsableL40, s.UsableL40Low = &sys.storedKWh, &sys.usableL40, &sys.usableL40Low
		if e.low != nil { // judged at the end of every input
			low := *e.low
			s.LowHotWater = &low
		}
	}
	s.Boiler = Boiler{HW: switchOf(e.hw), CH: switchOf(e.ch)}
	if e.ambOK && e.now.Sub(e.ambAt) <= e.cfg.Thermal.AmbientStale {
		c := e.ambC
		s.AmbientC = &c
	}
	if d, ok := e.meter.Draw(); ok {
		s.ActiveDraw = activeOf(d)
	} else if len(e.held) > 0 {
		s.ActiveDraw = activeOf(e.held[0])
	}
	if e.meterSeen {
		at, n, since, l := e.meterAt, e.meterCount, e.countingSince, e.meter.Health().Litres
		s.Meter = MeterState{Count: &n, CountingSince: &since, Litres: &l}
		if !at.IsZero() {
			s.Meter.At = &at
		}
		if f, ok := e.meter.Flow(); ok {
			x := flowOf(f)
			s.Meter.ContinuousFlow = &x
		}
	}
	return s
}

// systemTotals is the system's stored energy and usable litres.
type systemTotals struct {
	storedKWh, usableL40, usableL40Low float64
}

// system is the totals, if every tank's estimate is known: the stored
// energy summed, the usable litres through the merged outlet.
func (e *Engine) system() (systemTotals, bool) {
	var out systemTotals
	if len(e.order) == 0 {
		return out, false
	}
	ps := make([]*profile.Profile, len(e.order))
	for k, id := range e.order {
		slot := e.tanks[id]
		if !e.estimated(slot) {
			return out, false
		}
		out.storedKWh += slot.last.Estimate.KWh
		ps[k] = slot.last.Profile
	}
	m, err := e.est.Merged(ps, e.cfg.OutletSplit)
	if err != nil {
		return out, false // can't happen: the split is validated
	}
	out.usableL40, out.usableL40Low = m.UsableL40, m.UsableL40Low
	return out, true
}

// estimated says whether slot's latest sweep was estimated and isn't stale.
func (e *Engine) estimated(slot *tankSlot) bool {
	return slot.seen && slot.ok && e.now.Sub(slot.last.At) <= e.cfg.Thermal.StaleAfter
}

// judgeLow updates low_hot_water's held value from the system's
// usable_l40_low, with hysteresis; while that is unknown the value is held.
func (e *Engine) judgeLow() {
	sys, ok := e.system()
	if !ok {
		return
	}
	u := sys.usableL40Low
	var low bool
	switch {
	case e.low == nil:
		low = u < e.cfg.LowHotWaterL
	case *e.low:
		low = !(u > e.cfg.LowHotWaterL+e.cfg.LowHotWaterMarginL)
	default:
		low = u < e.cfg.LowHotWaterL
	}
	e.low = &low
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
	if !e.estimated(slot) {
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
// first, each with its balance once given. The slice is the caller's.
func (e *Engine) Recent() []DrawRecord {
	out := append([]DrawRecord(nil), e.recent...)
	for i := range out {
		if b := out[i].Balanced; b != nil {
			x := *b
			out[i].Balanced = &x
		}
	}
	return out
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
