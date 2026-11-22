package thermal

import (
	"math"
	"strings"
	"testing"
	"time"
)

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

// A steady gain shows as its own power once the window spans PowerMinSpan;
// before that the power is unknown.
func TestPowerIsTheSlope(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	s.ramp(tr, left, t0, 4*time.Minute+30*time.Second, 30*time.Second, 40, 3)
	if p := mustStatus(t, tr, left).PowerKW; p != nil {
		t.Fatalf("power after 4.5 min (min span 5): %v, want unknown", *p)
	}
	s.ramp(tr, left, t0, 12*time.Minute, 30*time.Second, 40, 3)
	p := mustStatus(t, tr, left).PowerKW
	if p == nil || !near(*p, 3, 0.01) {
		t.Fatalf("power %v, want 3 kW", p)
	}
}

// Only the last PowerWindow counts: a reheat that stopped is forgotten.
func TestPowerWindowForgets(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	_, c := s.ramp(tr, left, t0, 20*time.Minute, 30*time.Second, 40, 5)
	s.ramp(tr, left, t0.Add(20*time.Minute), 11*time.Minute, 30*time.Second, c, -0.2)
	p := mustStatus(t, tr, left).PowerKW
	if p == nil || !near(*p, -0.2, 0.01) {
		t.Fatalf("power %v, want −0.2 kW", p)
	}
}

// Probes are quantised to 1/16 °C; the window's slope still finds a slow
// loss.
func TestPowerThroughQuantisation(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	for at := t0; at.Before(t0.Add(15 * time.Minute)); at = at.Add(30 * time.Second) {
		c := 50 - 0.15/kwhPerK*at.Sub(t0).Hours()
		c = math.Round(c*16) / 16
		if _, err := tr.Sweep(s.sweep(left, at, c)); err != nil {
			t.Fatal(err)
		}
	}
	p := mustStatus(t, tr, left).PowerKW
	if p == nil || !near(*p, -0.15, 0.05) {
		t.Fatalf("power %v, want −0.15 ± 0.05 kW", p)
	}
}

// The inlet and outlet probes answer to flow, not heat: with them excluded,
// the outlet fitting warming after a draw is no gain.
func TestPowerExcludesFittings(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	for at := t0; at.Before(t0.Add(12 * time.Minute)); at = at.Add(30 * time.Second) {
		top, bottom := 45.0, 20.0
		if at.Sub(t0) >= 6*time.Minute {
			top, bottom = 55, 25 // the fittings re-warm; the water doesn't
		}
		if _, err := tr.Sweep(s.sweepT(right, at, []float64{bottom, 40, 50, 48, top})); err != nil {
			t.Fatal(err)
		}
	}
	p := mustStatus(t, tr, right).PowerKW
	if p == nil || !near(*p, 0, 0.001) {
		t.Fatalf("power %v, want 0", p)
	}
	// With nothing excluded the power is the sweep's own estimate's slope,
	// fittings and all.
	cfg := planConfig()
	cfg.PowerExclude = nil
	tr = newTracker(t, cfg)
	for at := t0; at.Before(t0.Add(12 * time.Minute)); at = at.Add(30 * time.Second) {
		top := 45.0
		if at.Sub(t0) >= 6*time.Minute {
			top = 55
		}
		if _, err := tr.Sweep(s.sweepT(right, at, []float64{20, 40, 50, 48, top})); err != nil {
			t.Fatal(err)
		}
	}
	if p := mustStatus(t, tr, right).PowerKW; p == nil || *p < 0.5 {
		t.Fatalf("power %v with the outlet counted, want a gain", p)
	}
}

// When the set of usable probes changes, the energy steps for no physical
// reason, so the window starts again.
func TestPowerRestartsWhenAProbeDropsOut(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	_, c := s.ramp(tr, left, t0, 12*time.Minute, 30*time.Second, 40, 2)
	if _, err := tr.Sweep(s.sweep(left, t0.Add(12*time.Minute), c, 2)); err != nil {
		t.Fatal(err)
	}
	if p := mustStatus(t, tr, left).PowerKW; p != nil {
		t.Fatalf("power %v straight after a probe dropped out, want unknown", *p)
	}
}

// A sweep from an unsynced clock, or older than the last, is not used; a
// sweep from a tank that isn't configured is an error.
func TestSweepRefusals(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	_, c := s.ramp(tr, left, t0, 12*time.Minute, 30*time.Second, 40, 2)
	before := mustStatus(t, tr, left)
	sw := s.sweep(left, t0.Add(13*time.Minute), c+20)
	sw.ClockSynced = false
	if _, err := tr.Sweep(sw); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Sweep(s.sweep(left, t0.Add(time.Minute), c+20)); err != nil {
		t.Fatal(err)
	}
	after := mustStatus(t, tr, left)
	if !after.At.Equal(before.At) || *after.PowerKW != *before.PowerKW {
		t.Fatalf("status moved: %+v → %+v", before, after)
	}
	sw = s.sweep(left, t0.Add(14*time.Minute), c)
	sw.DeviceID = "hot_water_cylinder_middle"
	if _, err := tr.Sweep(sw); err == nil || !strings.Contains(err.Error(), "middle") {
		t.Fatalf("unknown tank: %v", err)
	}
	if _, ok := tr.Tank("hot_water_cylinder_middle"); ok {
		t.Fatal("status for an unknown tank")
	}
}
