package draw

import (
	"time"

	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/tank"
	"github.com/sweeney/washhouse/internal/wire/boiler"
)

// Stage says what a Result reports.
type Stage int

// The stages, in the order a draw goes through them.
const (
	// StageEnded: the meter has ended the draw and every tank's share sweep
	// is in (draw_ended). Result.Draw is the meter's draw.
	StageEnded Stage = iota + 1
	// StageBalanced: the attribution is complete (draw_balanced).
	// Result.Attribution is set, and Result.Draw is its draw.
	StageBalanced
)

// Result is one stage of one draw.
type Result struct {
	Stage       Stage
	Draw        meter.Draw
	Attribution Attribution // StageBalanced only
}

// Attributor buffers what Attribute needs and gives each ended draw in two
// stages. The engine feeds it every tank sweep, every meter event and every
// programmer HW change, in arrival order, and calls Tick on its clock
// (event time); each call returns the results it completed, every draw's
// StageEnded before its StageBalanced, oldest draw first within a stage.
//
// A draw is ended (StageEnded) once the meter has ended it and every tank
// has a sweep at or after the last litre + ShareSettle; or once event time
// is past that + MaxSweepGap; or as soon as the meter starts the next draw
// (the end carries nothing the sweeps give, so nothing is lost, and the
// engine's active draw moves straight on to the next).
//
// It is balanced (StageBalanced) once every tank has a sweep at or after
// its balance target (the last litre + BalanceSettle), or at or after the
// next draw's first litre if that comes first; or, failing that, once
// event time is past the balance target + MaxSweepGap, when it is
// attributed with what there is. A next draw the meter reports only after
// a draw was balanced (a slow first litre can hold draw_started back by up
// to a minute) doesn't cut it.
//
// It keeps each tank's sweeps from MaxSweepGap before the oldest draw it
// still needs (active or pending), or before the latest event time when
// there is none, plus the one sweep before that. It is not safe for
// concurrent use.
type Attributor struct {
	cfg     Config
	sweeps  map[string][]tank.Sweep
	hw      []Programmer
	active  *meter.Draw // started, not ended
	pending []meter.Draw
	told    map[string]bool      // by pending draw ID: StageEnded given
	cuts    map[string]time.Time // by pending draw ID: the next draw's first litre
	now     time.Time
}

// New validates cfg and returns an empty Attributor.
func New(cfg Config) (*Attributor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	a := &Attributor{cfg: cfg, sweeps: map[string][]tank.Sweep{}, told: map[string]bool{}, cuts: map[string]time.Time{}}
	for _, id := range cfg.Tanks {
		a.sweeps[id] = nil
	}
	return a, nil
}

// Sweep takes one tank sweep. A sweep from a tank not in the config, or
// not after that tank's last, is ignored.
func (a *Attributor) Sweep(s tank.Sweep) []Result {
	ss, ok := a.sweeps[s.DeviceID]
	if !ok || (len(ss) > 0 && !s.At.After(ss[len(ss)-1].At)) {
		return nil
	}
	a.sweeps[s.DeviceID] = append(ss, s)
	return a.advance(s.At)
}

// HW takes the programmer's HW state from at on (from a heartbeat or an
// event). A state no later than the last is ignored.
func (a *Attributor) HW(at time.Time, hw boiler.Switch) {
	if n := len(a.hw); n > 0 && !at.After(a.hw[n-1].At) {
		return
	}
	if n := len(a.hw); n > 0 && a.hw[n-1].HW == hw {
		return
	}
	a.hw = append(a.hw, Programmer{At: at, HW: hw})
}

// Meter takes one meter event: draw_started and draw_ended matter, the
// rest are ignored.
func (a *Attributor) Meter(ev meter.Event) []Result {
	if ev.Draw == nil {
		return nil
	}
	d := *ev.Draw
	switch ev.Type {
	case meter.EventDrawStarted:
		var out []Result
		for _, p := range a.pending {
			if _, ok := a.cuts[p.ID]; !ok && d.Start.After(p.End) {
				a.cuts[p.ID] = d.Start
			}
			if !a.told[p.ID] {
				a.told[p.ID] = true
				out = append(out, Result{Stage: StageEnded, Draw: p})
			}
		}
		a.active = &d
		return append(out, a.advance(ev.At)...)
	case meter.EventDrawEnded:
		if a.active != nil && a.active.ID == d.ID {
			a.active = nil
		}
		a.pending = append(a.pending, d)
	default:
		return nil
	}
	return a.advance(ev.At)
}

// Tick advances event time to now and gives any draw whose wait is over.
func (a *Attributor) Tick(now time.Time) []Result { return a.advance(now) }

// Pending is how many draws the meter has ended that aren't yet balanced.
func (a *Attributor) Pending() int { return len(a.pending) }

// Buffered is how many sweeps are held, over all tanks.
func (a *Attributor) Buffered() int {
	n := 0
	for _, ss := range a.sweeps {
		n += len(ss)
	}
	return n
}

func (a *Attributor) advance(t time.Time) []Result {
	if t.After(a.now) {
		a.now = t
	}
	var out []Result
	for _, p := range a.pending {
		if !a.told[p.ID] && a.endReady(p) {
			a.told[p.ID] = true
			out = append(out, Result{Stage: StageEnded, Draw: p})
		}
	}
	for len(a.pending) > 0 && a.ready(a.pending[0]) {
		d := a.pending[0]
		a.pending = a.pending[1:]
		if !a.told[d.ID] { // can't happen: the balance waits longer
			out = append(out, Result{Stage: StageEnded, Draw: d})
		}
		at := Attribute(a.cfg, d, Window{Sweeps: a.sweeps, HW: a.hw, Cut: a.cuts[d.ID]})
		out = append(out, Result{Stage: StageBalanced, Draw: d, Attribution: at})
		delete(a.cuts, d.ID)
		delete(a.told, d.ID)
	}
	a.prune()
	return out
}

// endReady says whether every tank has a sweep at or after d's share
// target, or the wait for them is over.
func (a *Attributor) endReady(d meter.Draw) bool {
	target := d.End.Add(a.cfg.ShareSettle)
	if a.now.After(target.Add(a.cfg.MaxSweepGap)) {
		return true
	}
	for _, ss := range a.sweeps {
		if len(ss) == 0 || ss[len(ss)-1].At.Before(target) {
			return false
		}
	}
	return true
}

// ready says whether every tank has a sweep past what d's balance needs,
// or d's wait is over.
func (a *Attributor) ready(d meter.Draw) bool {
	target := d.End.Add(a.cfg.BalanceSettle)
	if a.now.After(target.Add(a.cfg.MaxSweepGap)) {
		return true
	}
	if cut, ok := a.cuts[d.ID]; ok && cut.Before(target) {
		target = cut
		if share := d.End.Add(a.cfg.ShareSettle); target.Before(share) {
			target = share
		}
	}
	for _, ss := range a.sweeps {
		if len(ss) == 0 || ss[len(ss)-1].At.Before(target) {
			return false
		}
	}
	return true
}

// prune drops what no draw can need any more.
func (a *Attributor) prune() {
	horizon := a.now
	if a.active != nil && a.active.Start.Before(horizon) {
		horizon = a.active.Start
	}
	for _, p := range a.pending {
		if p.Start.Before(horizon) {
			horizon = p.Start
		}
	}
	horizon = horizon.Add(-a.cfg.MaxSweepGap)
	for id, ss := range a.sweeps {
		a.sweeps[id] = keepFrom(ss, horizon, func(s tank.Sweep) time.Time { return s.At })
	}
	a.hw = keepFrom(a.hw, horizon, func(p Programmer) time.Time { return p.At })
}

// keepFrom drops the items before horizon except the last of them.
func keepFrom[T any](xs []T, horizon time.Time, at func(T) time.Time) []T {
	i := 0
	for i+1 < len(xs) && !at(xs[i+1]).After(horizon) {
		i++
	}
	if i == 0 {
		return xs
	}
	return append(xs[:0:0], xs[i:]...)
}
