package tank

import (
	"math"
	"testing"
	"time"
)

// The outlet probe is heat-sunk: at rest it reads low, and only a draw
// shows the water's true temperature (the owner; HOT-WATER-SYSTEM.md). The
// tank adds a learned offset to it for the estimate, none while water
// flows, ramping in over the time the offset was measured across after a
// draw. The sweep's probes stay raw: the draw attribution reads them.
func TestOutletOffset(t *testing.T) {
	// The outlet (top) is the warmest probe, so it sets the profile's top.
	st := state(f(20), f(25), f(38), f(34), f(40))
	end := at.Add(-time.Hour)

	base := mustTank(t, rightTank())
	b, err := base.Sweep(st)
	if err != nil {
		t.Fatal(err)
	}
	if b.Estimate.TopC != 40 {
		t.Fatalf("no offset: top %v, want the raw 40", b.Estimate.TopC)
	}

	cases := []struct {
		name    string
		offset  float64
		running bool
		ended   time.Time
		wantTop float64
	}{
		{"no offset learned", 0, false, end, 40},
		{"at rest, long after a draw: the whole offset", 6, false, end, 46},
		{"water running: the probe reads true", 6, true, end, 40},
		{"halfway through the ramp", 6, false, at.Add(-4 * time.Minute), 43},
		{"just as the draw ended", 6, false, at, 40},
		{"a negative offset is no offset", -3, false, end, 40},
	}
	for _, c := range cases {
		tk := mustTank(t, rightTank())
		tk.SetOutletOffset(c.offset, 8*time.Minute)
		tk.Flow(c.running, c.ended)
		sw, err := tk.Sweep(st)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(sw.Estimate.TopC-c.wantTop) > 1e-9 {
			t.Errorf("%s: top %v, want %v", c.name, sw.Estimate.TopC, c.wantTop)
		}
		if c.wantTop > 40 && !(sw.Estimate.UsableL40 > b.Estimate.UsableL40) {
			t.Errorf("%s: usable %.1f L, want more than the raw %.1f L", c.name, sw.Estimate.UsableL40, b.Estimate.UsableL40)
		}
		if o := sw.Outlet(); o == nil || *o != 40 {
			t.Errorf("%s: the sweep's outlet reading %v, want the raw 40 (the attribution reads it)", c.name, o)
		}
		if sw.Probes[4].TempC == nil || *sw.Probes[4].TempC != 40 {
			t.Errorf("%s: the sweep's outlet probe %v, want the raw 40", c.name, sw.Probes[4].TempC)
		}
	}
}

// Before any draw has been seen, no offset applies, whatever is set.
func TestOutletOffsetNeedsADraw(t *testing.T) {
	tk := mustTank(t, rightTank())
	tk.SetOutletOffset(6, 8*time.Minute)
	sw, err := tk.Sweep(state(f(20), f(25), f(38), f(34), f(40)))
	if err != nil {
		t.Fatal(err)
	}
	if sw.Estimate.TopC != 40 {
		t.Errorf("top %v with no draw seen, want the raw 40", sw.Estimate.TopC)
	}
}
