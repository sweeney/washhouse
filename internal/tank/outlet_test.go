package tank

import (
	"math"
	"testing"
	"time"
)

// The outlet probe is heat-sunk: at rest it reads low, and only a draw
// shows the water's true temperature (the owner; HOT-WATER-SYSTEM.md). How
// low scales with how far the water is above the cold feed (on 24 Nov 2026
// the left read 8.5 °C low at 62 and the right 6 at 46: both 0.16–0.17 of
// the way down to the 10 °C feed), so the tank keeps that fraction, k: the
// configured prior until it has learned some, then the median of the last
// OffsetWindow learned (one odd draw can't replace a good value:
// tanktemp's review of #12). For the estimate the outlet reads
// k/(1−k) × (its reading − ColdC) higher: none while water flows, ramping
// in after a draw. The sweep's probes stay raw: the draw attribution reads
// them. testEstimator's cold feed is 10 °C, so at k 0.2 an outlet reading
// 40 is 7.5 °C low, and at 0.25, 10.
func TestOutletUnderread(t *testing.T) {
	warm := state(f(20), f(25), f(38), f(34), f(40))
	cold := state(f(10), f(10), f(10), f(10), f(10))
	end := at.Add(-time.Hour)
	cases := []struct {
		name    string
		prior   float64
		learned []float64
		running bool
		ended   time.Time
		cold    bool
		wantTop float64
	}{
		{name: "no prior, nothing learned", ended: end, wantTop: 40},
		{name: "the prior, before any draw", prior: 0.2, wantTop: 47.5},
		{name: "the prior, long after a draw", prior: 0.2, ended: end, wantTop: 47.5},
		{name: "one learned replaces the prior", prior: 0.2, learned: []float64{0.25}, ended: end, wantTop: 50},
		{name: "the median of what was learned", prior: 0.1, learned: []float64{0.2, 0.05, 0.25}, ended: end, wantTop: 47.5},
		{name: "an odd one doesn't move the median", prior: 0.1, learned: []float64{0.2, 0.2, 0.05}, ended: end, wantTop: 47.5},
		{name: "only the last OffsetWindow count", prior: 0.1, learned: []float64{0.05, 0.05, 0.05, 0.25, 0.25, 0.25, 0.25, 0.25}, ended: end, wantTop: 50},
		{name: "water running: the probe reads true", prior: 0.2, running: true, ended: end, wantTop: 40},
		{name: "halfway through the ramp", prior: 0.2, ended: at.Add(-4 * time.Minute), wantTop: 43.75},
		{name: "just as the draw ended", prior: 0.2, ended: at, wantTop: 40},
		{name: "a fraction outside (0, 1) is ignored", prior: 0.2, learned: []float64{-0.1, 0, 1, 1.5}, ended: end, wantTop: 47.5},
		{name: "a cold tank: nothing to under-read", prior: 0.2, ended: end, cold: true, wantTop: 10},
	}
	for _, c := range cases {
		cfg := rightTank()
		cfg.PriorOutletUnderread = c.prior
		tk := mustTank(t, cfg)
		tk.SetRamp(8 * time.Minute)
		for _, x := range c.learned {
			tk.LearnOutletUnderread(x)
		}
		tk.Flow(c.running, c.ended)
		st, raw := warm, 40.0
		if c.cold {
			st, raw = cold, 10
		}
		sw, err := tk.Sweep(st)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(sw.Estimate.TopC-c.wantTop) > 1e-9 {
			t.Errorf("%s: top %v, want %v", c.name, sw.Estimate.TopC, c.wantTop)
		}
		if o := sw.Outlet(); o == nil || *o != raw {
			t.Errorf("%s: the sweep's outlet reading %v, want the raw %v (the attribution reads it)", c.name, o, raw)
		}
	}
}

// What was learned can be handed to a new tank (a reload rebuilds the
// engine).
func TestOutletUnderreadCarriesOver(t *testing.T) {
	a := mustTank(t, rightTank())
	for _, x := range []float64{0.2, 0.1, 0.25} {
		a.LearnOutletUnderread(x)
	}
	b := mustTank(t, rightTank())
	b.SetOutletUnderreads(a.OutletUnderreads())
	sw, err := b.Sweep(state(f(20), f(25), f(38), f(34), f(40)))
	if err != nil {
		t.Fatal(err)
	}
	if sw.Estimate.TopC != 47.5 {
		t.Errorf("carried over: top %v, want 40 + the median 0.2's 7.5", sw.Estimate.TopC)
	}
}

func TestPriorOutletUnderreadValidated(t *testing.T) {
	for _, x := range []float64{-0.1, 1, math.NaN(), math.Inf(1)} {
		c := rightTank()
		c.PriorOutletUnderread = x
		if c.Validate() == nil {
			t.Errorf("prior outlet under-read %v accepted", x)
		}
	}
}
