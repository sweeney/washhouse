package engine

import (
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
// right's at 47, masked by a reheat or not.
func balanced(d meter.Draw, masked bool) draw.Result {
	l, r := 63.0, 47.0
	return draw.Result{Stage: draw.StageBalanced, Draw: d, Attribution: draw.Attribution{
		MaskedByReheat: masked,
		Tanks:          []draw.TankAttribution{{DeviceID: left, OutletPeakC: &l}, {DeviceID: right, OutletPeakC: &r}},
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

// A rested, flowing, unmasked draw teaches each tank its offset (peak less
// the reading at rest just before: 63 − 55 and 47 − 41); a reheat-masked
// one, a one-litre one, or one that didn't follow a rest teaches nothing.
func TestOutletOffsetLearning(t *testing.T) {
	cases := []struct {
		name         string
		litres       float64
		masked       bool
		restless     bool
		wantL, wantR float64
	}{
		{"rested, flowing, unmasked", 6, false, false, 63, 47},
		{"masked by a reheat", 6, true, false, 55, 41},
		{"one litre", 1, false, false, 55, 41},
		{"no rest before it", 6, false, true, 55, 41},
	}
	for _, c := range cases {
		e := newEngine(t, planConfig())
		d := rested(t, e, c.litres)
		if c.restless {
			e.lastDrawEnd = d.Start.Add(-time.Minute)
		}
		e.drawStarts(d)
		e.learnOutlet(balanced(d, c.masked))
		if l, r := topAfter(t, e, d); l != c.wantL || r != c.wantR {
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
