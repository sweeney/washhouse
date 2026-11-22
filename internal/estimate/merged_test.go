package estimate

import (
	"testing"

	"github.com/sweeney/washhouse/internal/profile"
)

// houseConfig is testConfig with the house's drawable band (the cold inlet
// to the hot outlet), so plug flow leaves water below the inlet behind.
func houseConfig() Config {
	c := testConfig()
	c.DrawFromMM, c.DrawToMM = 281, 1237
	return c
}

// One tank alone through the outlet is the tank's own plug-flow estimate,
// for a profile hot at the top and cooling downwards (the hot water is one
// layer, so counting it slice by slice or drawing it from the top agree).
func TestMergedOneTankIsItsOwnEstimate(t *testing.T) {
	e := mustEstimator(t, houseConfig())
	for _, p := range []*profile.Profile{
		prof(t, false, 60, 60, 60, 60, 60),
		prof(t, false, 15, 30, 55, 60, 62),
		prof(t, false, 15, 15, 20, 38, 41),
	} {
		want := e.Assess(p)
		got, err := e.Merged([]*profile.Profile{p}, []float64{1})
		if err != nil {
			t.Fatal(err)
		}
		near(t, "UsableL40", got.UsableL40, want.UsableL40, 0.05)
		near(t, "UsableL40Low", got.UsableL40Low, want.UsableL40Low, 0.05)
		near(t, "FirstC", got.FirstC, p.At(1236.5), 0.5)
	}
}

// Two equal tanks split evenly deliver what both hold.
func TestMergedEqualTanksSum(t *testing.T) {
	e := mustEstimator(t, houseConfig())
	p := prof(t, false, 15, 30, 55, 60, 62)
	one := e.Assess(p).UsableL40
	got, err := e.Merged([]*profile.Profile{p, p}, []float64{0.5, 0.5})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "UsableL40", got.UsableL40, 2*one, 0.1)
}

// A hot tank and a cold one: the tap gets the split's mix. At 0.75 hot the
// mix (47.5 °C) is usable until the hot tank's band is spent, which takes
// band/0.75 litres through the outlet; at 0.25 hot (22.5 °C) the first
// litre is already too cool and nothing is usable, however full the hot
// tank is.
func TestMergedHotAndColdTank(t *testing.T) {
	c := houseConfig()
	e := mustEstimator(t, c)
	hot, cold := prof(t, false, 60, 60, 60, 60, 60), prof(t, false, 10, 10, 10, 10, 10)
	band := e.Assess(hot).UsableL40 * (c.MixC - c.ColdC) / (60 - c.ColdC) // the band's litres
	got, err := e.Merged([]*profile.Profile{hot, cold}, []float64{0.75, 0.25})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "FirstC", got.FirstC, 47.5, 1e-9)
	near(t, "Litres", got.Litres, band/0.75, 1e-6)
	near(t, "UsableL40", got.UsableL40, band/0.75*(47.5-10)/30, 1e-6)
	near(t, "UsableL40Low", got.UsableL40Low, 0.85*band/0.75*(47.5-10)/30, 1e-6)
	got, err = e.Merged([]*profile.Profile{hot, cold}, []float64{0.25, 0.75})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "FirstC", got.FirstC, 22.5, 1e-9)
	if got.UsableL40 != 0 || got.Litres != 0 {
		t.Errorf("usable %v L over %v L at a 22.5 °C mix, want none", got.UsableL40, got.Litres)
	}
}

// The mix stops counting when it falls below MinC, not when one tank runs
// cold: the hotter tank carries the mix for a while.
func TestMergedStopsAtMinC(t *testing.T) {
	c := houseConfig()
	e := mustEstimator(t, c)
	hot := prof(t, false, 70, 70, 70, 70, 70)
	warm := prof(t, false, 10, 10, 10, 45, 45) // hot only near the top
	got, err := e.Merged([]*profile.Profile{hot, warm}, []float64{0.5, 0.5})
	if err != nil {
		t.Fatal(err)
	}
	// After the warm tank's top is spent the mix is (70 + 10) / 2 = 40,
	// still at MinC; it never falls below it until the hot tank is spent.
	hotBand := e.Assess(hot).UsableL40 * 30 / 60
	near(t, "Litres", got.Litres, hotBand/0.5, 1e-6)
	if got.FirstC < 57 || got.FirstC > 58 {
		t.Errorf("first litre %v °C, want about (70+45)/2", got.FirstC)
	}
}

func TestMergedRefuses(t *testing.T) {
	e := mustEstimator(t, houseConfig())
	p := prof(t, false, 60, 60, 60, 60, 60)
	for name, tc := range map[string]struct {
		ps    []*profile.Profile
		split []float64
	}{
		"no tanks":         {nil, nil},
		"lengths differ":   {[]*profile.Profile{p, p}, []float64{1}},
		"nil profile":      {[]*profile.Profile{p, nil}, []float64{0.5, 0.5}},
		"negative split":   {[]*profile.Profile{p, p}, []float64{1.5, -0.5}},
		"not summing to 1": {[]*profile.Profile{p, p}, []float64{0.5, 0.4}},
	} {
		if _, err := e.Merged(tc.ps, tc.split); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
