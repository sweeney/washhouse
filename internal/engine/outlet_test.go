package engine

import (
	"math"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/meter"
)

// rested gives e a sweep of each tank (the left's outlet at 55 °C, the
// right's at 41) and a draw starting UAQuiet after them, so it followed a
// rest; it returns the draw.
func rested(t *testing.T, e *Engine, litres float64) meter.Draw {
	t.Helper()
	lowFeed(t, e, t0, rightLow)
	start := t0.Add(e.cfg.Thermal.UAQuiet)
	d := meter.Draw{ID: "d1", Start: start, End: start.Add(time.Minute), Litres: litres}
	if litres >= 2 {
		x := 6.0
		d.FlowLPerMin = &x
	}
	return d
}

// balanced is d balanced with the left outlet peaking at 63 °C and the
// right's at 47, half the litres from each, masked by a reheat or not.
func balanced(d meter.Draw, masked bool) draw.Result {
	l, r, half := 63.0, 47.0, d.Litres/2
	return draw.Result{Stage: draw.StageBalanced, Draw: d, Attribution: draw.Attribution{
		MaskedByReheat: masked,
		Tanks: []draw.TankAttribution{{DeviceID: left, OutletPeakC: &l, Litres: &half},
			{DeviceID: right, OutletPeakC: &r, Litres: &half}},
	}}
}

// topAfter is each tank's estimate top, an hour after d ended, at rest.
func topAfter(t *testing.T, e *Engine, d meter.Draw) (lt, rt float64) {
	t.Helper()
	for _, slot := range e.tanks {
		slot.model.Flow(false, d.End)
	}
	lowFeed(t, e, d.End.Add(e.cfg.Thermal.UAQuiet+time.Minute), rightLow)
	return e.tanks[left].last.Estimate.TopC, e.tanks[right].last.Estimate.TopC
}

// A rested, flowing, unmasked draw teaches each tank its under-read (the
// reading at rest just before, 55 and 41, below the peak, 63 and 47), so
// the outlet then reads the peak; a reheat-masked draw, a one-litre one, or
// one that didn't follow a rest teaches nothing, and the prior holds
// (planConfig's 0.16 and 0.17: 55 + 0.16/0.84 × 45 and 41 + 0.17/0.83 × 31).
func TestOutletOffsetLearning(t *testing.T) {
	priorL, priorR := 55+0.16/0.84*45, 41+0.17/0.83*31
	cases := []struct {
		name         string
		litres       float64
		masked       bool
		restless     bool
		wantL, wantR float64
	}{
		{"rested, flowing, unmasked", 6, false, false, 63, 47},
		{"masked by a reheat", 6, true, false, priorL, priorR},
		{"one litre", 1, false, false, priorL, priorR},
		{"no rest before it", 6, false, true, priorL, priorR},
	}
	for _, c := range cases {
		e := newEngine(t, planConfig())
		d := rested(t, e, c.litres)
		if c.restless {
			e.lastDrawEnd = d.Start.Add(-time.Minute)
		}
		e.drawStarts(d)
		e.learnOutlet(balanced(d, c.masked))
		if l, r := topAfter(t, e, d); math.Abs(l-c.wantL) > 1e-9 || math.Abs(r-c.wantR) > 1e-9 {
			t.Errorf("%s: tops %v / %v °C, want %v / %v", c.name, l, r, c.wantL, c.wantR)
		}
	}
}

// While water flows the outlet reads true, so no offset is added.
func TestOutletOffsetNotWhileFlowing(t *testing.T) {
	e := newEngine(t, planConfig())
	d := rested(t, e, 6)
	e.drawStarts(d)
	e.learnOutlet(balanced(d, false))
	// The first draw ended, as the meter's draw_ended tells the engine.
	e.lastDrawEnd = d.End
	for _, slot := range e.tanks {
		slot.model.Flow(false, d.End)
	}
	e.drawStarts(meter.Draw{ID: "d2", Start: d.End.Add(2 * time.Hour)})
	lowFeed(t, e, d.End.Add(2*time.Hour+time.Second), rightLow)
	if l, r := e.tanks[left].last.Estimate.TopC, e.tanks[right].last.Estimate.TopC; l != 55 || r != 41 {
		t.Errorf("tops while flowing %v / %v °C, want the raw 55 / 41", l, r)
	}
}

// A reload rebuilds the engine: the new one takes what the old learned,
// and the draw state, so the outlet correction doesn't jump.
func TestOutletTakenOverOnReload(t *testing.T) {
	old := newEngine(t, planConfig())
	d := rested(t, old, 6)
	old.drawStarts(d)
	old.learnOutlet(balanced(d, false))
	old.lastDrawEnd = d.End
	for _, slot := range old.tanks {
		slot.model.Flow(false, d.End)
	}
	e := newEngine(t, planConfig())
	e.TakeOutlet(old)
	if l, r := topAfter(t, e, d); l != 63 || r != 47 {
		t.Errorf("after a reload: tops %v / %v °C, want what the old engine learned (63 / 47)", l, r)
	}
	if !e.lastDrawEnd.Equal(d.End) {
		t.Errorf("last draw end %v, want %v", e.lastDrawEnd, d.End)
	}
}
