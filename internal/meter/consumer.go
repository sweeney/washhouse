package meter

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/sweeney/common/wire/tanktemp"
)

// Validate checks every threshold is set and consistent.
func (c Config) Validate() error {
	var errs []error
	if c.DeviceID == "" {
		errs = append(errs, errors.New("DeviceID: must be set"))
	}
	pos := func(name string, d time.Duration) {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s %v: must be > 0", name, d))
		}
	}
	posf := func(name string, f float64) {
		if !(f > 0) || math.IsInf(f, 0) {
			errs = append(errs, fmt.Errorf("%s %v: must be a finite number > 0", name, f))
		}
	}
	pos("DrawEnd", c.DrawEnd)
	posf("BathFlowLPerMin", c.BathFlowLPerMin)
	posf("BathMinLitres", c.BathMinLitres)
	posf("ShowerMinLitres", c.ShowerMinLitres)
	pos("ShowerMinDuration", c.ShowerMinDuration)
	posf("ShowerMinFlowLPerMin", c.ShowerMinFlowLPerMin)
	pos("CreepClosed", c.CreepClosed)
	pos("CreepWindow", c.CreepWindow)
	pos("StaleAfter", c.StaleAfter)
	if c.UpdateEvery < 0 {
		errs = append(errs, fmt.Errorf("UpdateEvery %v: must be >= 0 (0 is off)", c.UpdateEvery))
	}
	if len(errs) > 0 {
		return fmt.Errorf("meter config: %w", errors.Join(errs...))
	}
	return nil
}

// kindOf classifies a draw from its litres, median flow and duration. A
// draw with no flow (one litre) is a tap.
func kindOf(cfg Config, litres float64, flow *float64, dur time.Duration) Kind {
	switch {
	case flow == nil:
		return KindTap
	case *flow >= cfg.BathFlowLPerMin && litres >= cfg.BathMinLitres:
		return KindBath
	case *flow >= cfg.ShowerMinFlowLPerMin && litres >= cfg.ShowerMinLitres && dur >= cfg.ShowerMinDuration:
		return KindShower
	}
	return KindTap
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// run is one counting run: everything since one counting_since.
type run struct {
	maxSeq   uint64 // the highest seq seen: anything at or below is not news
	maxCount uint64 // count as of maxSeq
}

// counted is a timed litre (with any untimed pulses revealed with it).
type counted struct {
	at     time.Time
	pulses int
	k      float64
	count  uint64 // the run's count with this litre
	total  float64
	ref    Ref
	closed *int64 // how long its reed stayed closed, if known
	// long: its reed is known to have stayed closed CreepClosed or more. It
	// is creep unless a litre follows within DrawEnd.
	long bool
}

func (l counted) litres() float64 { return float64(l.pulses) * l.k }

type drawState struct {
	d          Draw
	secPerL    []float64 // seconds per litre between consecutive timed litres
	lastUpdate time.Time
}

type flowState struct {
	f       ContinuousFlow
	secPerL []float64
}

// Consumer is the meter consumer. It is not safe for concurrent use: one
// goroutine feeds it.
type Consumer struct {
	cfg       Config
	runs      map[int64]*run // by counting_since, in Unix ns
	cur       *run
	now       time.Time // event time: only ever moves forward
	lastLitre time.Time // the latest timed litre's closing
	pend      *counted  // a litre that would start a draw, until its closed time settles creep or not
	draw      *drawState
	flow      *flowState
	lastCreep *counted
	h         Health
}

// New returns a consumer with cfg, which must validate.
func New(cfg Config) (*Consumer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Consumer{cfg: cfg, runs: map[int64]*run{}}, nil
}

// msgTime is the event time a message speaks for, as exactly as it says it.
func msgTime(m tanktemp.Meter) time.Time {
	switch m.Trigger {
	case tanktemp.TriggerPulse:
		if m.LastPulseAt != nil {
			return *m.LastPulseAt
		}
	case tanktemp.TriggerOpen:
		if at, ok := m.OpenedAt(); ok {
			return at
		}
	}
	return m.Timestamp
}

// Feed takes the next meter message in arrival order and returns the
// events it decided.
func (c *Consumer) Feed(m tanktemp.Meter) []Event {
	c.h.Messages++
	if m.DeviceID != c.cfg.DeviceID {
		c.h.OtherDevices++
		return nil
	}
	var ev []Event
	key := m.CountingSince.UnixNano()
	r := c.runs[key]
	switch {
	case r == nil:
		// A new run: tanktemp restarted (or this is the first message).
		// A litre still waiting to be settled can't be any more: creep if
		// its reed was known to stay closed, else a draw's, since count is
		// the truth for volume.
		if c.pend != nil {
			ev = c.settle(ev, c.pend.long, c.pend.closed)
		}
		r = &run{}
		if m.Seq > 1 && c.cur == nil {
			// A cold start joined mid-run: what was counted before is the
			// baseline. (After a run this consumer saw, a restart it lived
			// through: the run counts from 0, so all of it is news, and the
			// seqs before this one are missing.)
			r.maxSeq, r.maxCount = m.Seq-1, m.Count
			if m.ClockSynced && m.LastPulseAt != nil && m.LastPulseAt.After(c.lastLitre) {
				c.lastLitre = *m.LastPulseAt
			}
		}
		c.runs[key] = r
		c.cur = r
		c.h.Runs++
	case r != c.cur:
		c.h.StaleRuns++
		return nil
	case m.Seq <= r.maxSeq:
		c.h.Duplicates++
		return nil
	}
	if m.Seq > r.maxSeq+1 {
		c.h.MissingSeq += m.Seq - r.maxSeq - 1
	}
	r.maxSeq = m.Seq
	c.health(m)
	if !m.ClockSynced {
		// The times can't be trusted (a Pi with no RTC, before NTP, runs
		// on the clock saved at shutdown, which can be earlier than the
		// last run's): count the litres, but time nothing from them.
		switch {
		case m.Count > r.maxCount:
			delta := int(m.Count - r.maxCount)
			r.maxCount = m.Count
			c.h.Litres += float64(delta) * m.LitresPerPulse
			c.h.Unsynced += delta
		case m.Count < r.maxCount:
			c.h.CountFell++
		}
		return ev
	}

	ev = c.evidence(ev, m)
	ev = c.advance(ev, msgTime(m))
	switch {
	case m.Count > r.maxCount:
		delta := int(m.Count - r.maxCount)
		r.maxCount = m.Count
		ev = c.pulses(ev, m, delta)
	case m.Count < r.maxCount:
		c.h.CountFell++
	}
	return ev
}

func (c *Consumer) health(m tanktemp.Meter) {
	c.h.Run = Ref{CountingSince: m.CountingSince, Seq: m.Seq}
	c.h.LastTimestamp = m.Timestamp
	c.h.ClockSynced = m.ClockSynced
	c.h.Level = m.Level
	c.h.Noise, c.h.Short, c.h.Glitches, c.h.Lost = m.Noise, m.Short, m.Glitches, m.Lost
	c.h.LostBoundLitres = 2 * float64(m.Lost) * m.LitresPerPulse
}

// pulses handles a rise in count of delta.
func (c *Consumer) pulses(ev []Event, m tanktemp.Meter, delta int) []Event {
	k := m.LitresPerPulse
	c.h.Litres += float64(delta) * k
	if delta > 1 {
		c.h.CountJumps++
	}
	ref := Ref{CountingSince: m.CountingSince, Seq: m.Seq}
	if m.LastPulseAt == nil || !m.LastPulseAt.After(c.lastLitre) {
		// No time for any of them: no last_pulse_at (can't happen within
		// the contract), or one no later than the last litre's (the synced
		// wall clock stepped back more than 50 ms, and last_pulse_at moved
		// with it). Join what's there.
		c.h.Skipped += delta
		switch {
		case c.draw != nil:
			d := &c.draw.d
			d.Pulses += delta
			d.Litres += float64(delta) * k
			d.To = ref
			ev = append(ev, Event{Type: EventLitre, At: m.Timestamp, Litre: &Litre{
				At: m.Timestamp, Pulses: delta, Skipped: delta, Litres: float64(delta) * k, Total: c.h.Litres,
				DrawID: d.ID, Ref: ref,
			}})
		case c.pend != nil:
			c.pend.pulses += delta
			c.pend.count = m.Count
			c.pend.total = c.h.Litres
		default:
			c.h.Unattributed += delta
		}
		return ev
	}
	c.h.Skipped += delta - 1
	l := counted{at: *m.LastPulseAt, pulses: delta, k: k, count: m.Count, total: c.h.Litres, ref: ref}
	c.lastLitre = l.at
	if m.Level == tanktemp.LevelOpen && m.LastClosedMS != nil {
		// the newest litre has ended (an open, or a sweep after it)
		cl := *m.LastClosedMS
		l.closed = &cl
	}
	return c.litre(ev, l, m)
}

// litre places a new timed litre: in the running draw, or as a litre that
// would start one.
func (c *Consumer) litre(ev []Event, l counted, m tanktemp.Meter) []Event {
	if p := c.pend; p != nil {
		// The reed closed again at l.at, so the waiting litre had opened.
		// Its closed time is this pulse's last_closed_ms when no pulse was
		// skipped between them.
		closed := p.closed
		if closed == nil && l.pulses == 1 && m.Level == tanktemp.LevelClosed && m.LastClosedMS != nil {
			cl := *m.LastClosedMS
			closed = &cl
		}
		// A litre within DrawEnd makes it a draw's first, however long it
		// was closed. Later than that (advance settles a long one first),
		// it's creep if known long, else a draw's (count is the truth).
		creep := l.at.Sub(p.at) >= c.cfg.DrawEnd && (p.long || closed != nil && c.isCreep(*closed))
		ev = c.settle(ev, creep, closed)
		ev = c.advance(ev, l.at) // a one-litre draw settled late may be over
	}
	if c.draw != nil {
		// advance has ended any draw DrawEnd or more before l.at
		return c.join(ev, l)
	}
	c.pend = &l
	return c.evidence(ev, m)
}

func (c *Consumer) isCreep(closedMS int64) bool {
	return time.Duration(closedMS)*time.Millisecond >= c.cfg.CreepClosed
}

// evidence settles the waiting litre from what m says about it. m speaks
// of it when the run's count is still the waiting litre's: last_pulse_at
// is not compared, since tanktemp can round it differently between a
// litre's pulse and its open.
func (c *Consumer) evidence(ev []Event, m tanktemp.Meter) []Event {
	p := c.pend
	if p == nil || m.CountingSince.UnixNano() != p.ref.CountingSince.UnixNano() || m.Count != p.count {
		return ev
	}
	switch {
	case m.Level == tanktemp.LevelOpen && m.LastClosedMS != nil:
		cl := *m.LastClosedMS
		p.closed = &cl
		if !c.isCreep(cl) {
			return c.settle(ev, false, &cl)
		}
		p.long = true
	case m.Level == tanktemp.LevelClosed && m.Timestamp.Sub(p.at) >= c.cfg.CreepClosed:
		p.long = true // still closed CreepClosed after it closed
	}
	// Long: creep, unless a litre follows within DrawEnd. advance decides
	// that, on this same message, once event time is DrawEnd past the
	// closing. A reed closed DrawEnd or more gets there on the message that
	// shows it: its open is timed at last_pulse_at + last_closed_ms exactly.
	// (A sweep after a lost open is timed to the second, so a reed closed
	// under a second past DrawEnd can wait one message more; the event's
	// time, the litre's closing, is the same.)
	return ev
}

// settle decides the waiting litre: creep, or the first litre of a draw.
func (c *Consumer) settle(ev []Event, creep bool, closed *int64) []Event {
	p := c.pend
	c.pend = nil
	p.closed = closed
	if creep {
		return c.creep(ev, *p)
	}
	c.draw = &drawState{
		d: Draw{ID: DrawID(p.ref), Start: p.at, End: p.at, Pulses: p.pulses, Litres: p.litres(),
			From: p.ref, To: p.ref},
		lastUpdate: p.at,
	}
	ev = append(ev, Event{Type: EventDrawStarted, At: p.at, Draw: c.drawSnapshot()})
	return append(ev, c.litreEvent(*p, false, c.draw.d.ID))
}

func (c *Consumer) litreEvent(l counted, creep bool, drawID string) Event {
	return Event{Type: EventLitre, At: l.at, Litre: &Litre{
		At: l.at, Pulses: l.pulses, Skipped: l.pulses - 1, Litres: l.litres(), Total: l.total,
		ClosedMS: l.closed, Creep: creep, DrawID: drawID, Ref: l.ref,
	}}
}

func (c *Consumer) join(ev []Event, l counted) []Event {
	ds := c.draw
	d := &ds.d
	ds.secPerL = append(ds.secPerL, l.at.Sub(d.End).Seconds()/l.litres())
	d.End = l.at
	d.Pulses += l.pulses
	d.Litres += l.litres()
	d.To = l.ref
	ev = append(ev, c.litreEvent(l, false, d.ID))
	if c.cfg.UpdateEvery > 0 && l.at.Sub(ds.lastUpdate) >= c.cfg.UpdateEvery {
		ds.lastUpdate = l.at
		ev = append(ev, Event{Type: EventDrawUpdated, At: l.at, Draw: c.drawSnapshot()})
	}
	return ev
}

func (c *Consumer) drawSnapshot() *Draw {
	ds := c.draw
	d := ds.d
	d.Duration = d.End.Sub(d.Start)
	d.FlowLPerMin = nil
	if len(ds.secPerL) > 0 {
		f := 60 / median(ds.secPerL)
		d.FlowLPerMin = &f
	}
	d.Kind = kindOf(c.cfg, d.Litres, d.FlowLPerMin, d.Duration)
	return &d
}

func (c *Consumer) creep(ev []Event, l counted) []Event {
	ev = append(ev, c.litreEvent(l, true, ""))
	switch {
	case c.flow != nil:
		fs := c.flow
		fs.secPerL = append(fs.secPerL, l.at.Sub(fs.f.Last).Seconds()/l.litres())
		fs.f.Last = l.at
		fs.f.Pulses += l.pulses
		fs.f.Litres += l.litres()
		fs.f.FlowLPerMin = 60 / median(fs.secPerL)
	case c.lastCreep != nil && l.at.Sub(c.lastCreep.at) <= c.cfg.CreepWindow:
		prev := c.lastCreep
		c.flow = &flowState{
			f: ContinuousFlow{Since: prev.at, Last: l.at, Pulses: prev.pulses + l.pulses,
				Litres: prev.litres() + l.litres()},
			secPerL: []float64{l.at.Sub(prev.at).Seconds() / l.litres()},
		}
		c.flow.f.FlowLPerMin = 60 / median(c.flow.secPerL)
		f := c.flow.f
		ev = append(ev, Event{Type: EventContinuousFlowStarted, At: l.at, Flow: &f})
	}
	c.lastCreep = &l
	return ev
}

// advance moves event time to t (never back) and ends what has run out,
// each at its own deadline.
func (c *Consumer) advance(ev []Event, t time.Time) []Event {
	if t.After(c.now) {
		c.now = t
	}
	if p := c.pend; p != nil && p.long && !c.now.Before(p.at.Add(c.cfg.DrawEnd)) {
		ev = c.settle(ev, true, p.closed) // long-closed, and no litre followed
	}
	var drawAt, flowAt time.Time
	endDraw := c.draw != nil && !c.now.Before(c.draw.d.End.Add(c.cfg.DrawEnd))
	endFlow := c.flow != nil && !c.now.Before(c.flow.f.Last.Add(c.cfg.CreepWindow))
	if endDraw {
		drawAt = c.draw.d.End.Add(c.cfg.DrawEnd)
	}
	if endFlow {
		flowAt = c.flow.f.Last.Add(c.cfg.CreepWindow)
	}
	if endFlow && endDraw && flowAt.Before(drawAt) {
		ev, endFlow = c.endFlow(ev, flowAt), false
	}
	if endDraw {
		d := c.drawSnapshot()
		d.Ended = true
		c.draw = nil
		ev = append(ev, Event{Type: EventDrawEnded, At: drawAt, Draw: d})
	}
	if endFlow {
		ev = c.endFlow(ev, flowAt)
	}
	return ev
}

func (c *Consumer) endFlow(ev []Event, at time.Time) []Event {
	f := c.flow.f
	f.Ended = true
	c.flow = nil
	return append(ev, Event{Type: EventContinuousFlowEnded, At: at, Flow: &f})
}

// Tick advances event time to t without a message, so a draw or a
// continuous flow can end. A t at or before the current event time moves
// nothing.
func (c *Consumer) Tick(t time.Time) []Event { return c.advance(nil, t) }

// Health reports the meter's health.
func (c *Consumer) Health() Health { return c.h }

// Fresh says whether the latest message is no older than StaleAfter at now
// (an event time the caller supplies).
func (c *Consumer) Fresh(now time.Time) bool {
	return !c.h.LastTimestamp.IsZero() && now.Sub(c.h.LastTimestamp) <= c.cfg.StaleAfter
}

// Draw returns the running draw, if any.
func (c *Consumer) Draw() (Draw, bool) {
	if c.draw == nil {
		return Draw{}, false
	}
	return *c.drawSnapshot(), true
}

// Flow returns the running continuous flow, if any.
func (c *Consumer) Flow() (ContinuousFlow, bool) {
	if c.flow == nil {
		return ContinuousFlow{}, false
	}
	return c.flow.f, true
}
