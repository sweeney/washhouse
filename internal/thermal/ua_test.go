package thermal

import (
	"math"
	"testing"
	"time"
)

// cwhPerK is a uniform STI 210's heat capacity in Wh/K.
const cwhPerK = kwhPerK * 1000

// night cools tank id for d from start, as a tank with loss ua (W/K) to a
// room at ambC, starting at fromC: T = amb + (T0 − amb)·exp(−ua·t/C). The
// room reports every 15 min. It returns the temperature at the end.
func night(t *testing.T, s *synth, tr *Tracker, id string, start time.Time, d time.Duration, fromC, ambC, ua float64) float64 {
	t.Helper()
	c := fromC
	for at := start; at.Before(start.Add(d)); at = at.Add(30 * time.Second) {
		el := at.Sub(start)
		if el%(15*time.Minute) == 0 {
			tr.Ambient(at, ambC)
		}
		c = ambC + (fromC-ambC)*math.Exp(-ua*el.Hours()/cwhPerK)
		if _, err := tr.Sweep(s.sweep(id, at, c)); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func uaOf(t *testing.T, tr *Tracker, id string) UA {
	t.Helper()
	return mustStatus(t, tr, id).UA
}

// Before any evidence the fit is the configured prior.
func TestUAStartsAtThePrior(t *testing.T) {
	tr := newTracker(t, planConfig())
	for _, tc := range planConfig().Tanks {
		u := uaOf(t, tr, tc.DeviceID)
		if u.WPerK != tc.PriorUAWPerK || u.EvidenceHours != 0 || u.PriorShare != 1 {
			t.Fatalf("%s: %+v", tc.DeviceID, u)
		}
	}
}

// A quiet night fits the tank's loss; the evidence starts UAQuiet after the
// tank is known to be cooling, and counts hours.
func TestUAFitsAQuietNight(t *testing.T) {
	cfg := planConfig()
	s := newSynth(t)
	tr := newTracker(t, cfg)
	night(t, s, tr, right, t0, 8*time.Hour, 50, 22, 5)
	u := uaOf(t, tr, right)
	// The prior (17 W/K, an hour's worth) still has a share: 1 h of 8.
	if !near(u.WPerK, 5, 1.6) || u.WPerK < 5 {
		t.Fatalf("UA %+v, want 5 W/K pulled a little toward the prior", u)
	}
	// Cooling is known after PowerMinSpan (5 min), and evidence starts
	// UAQuiet (60 min) after that.
	want := 8 - (5*time.Minute + cfg.UAQuiet).Hours()
	if !near(u.EvidenceHours, want, 0.02) {
		t.Fatalf("evidence %.3f h, want %.3f", u.EvidenceHours, want)
	}
	if u.PriorShare <= 0 || u.PriorShare > 0.15 {
		t.Fatalf("prior share %.3f", u.PriorShare)
	}
}

// The fit forgets: after a step in the loss (the right tank's loop
// stopping, then restarting) it follows within a night or two.
func TestUAFollowsAStep(t *testing.T) {
	s := newSynth(t)
	tr := newTracker(t, planConfig())
	day := 24 * time.Hour
	night(t, s, tr, right, t0, 8*time.Hour, 50, 22, 5)
	first := uaOf(t, tr, right).WPerK
	night(t, s, tr, right, t0.Add(day), 8*time.Hour, 55, 22, 17)
	second := uaOf(t, tr, right).WPerK
	night(t, s, tr, right, t0.Add(2*day), 8*time.Hour, 55, 22, 17)
	third := uaOf(t, tr, right).WPerK
	t.Logf("UA after each night: %.2f %.2f %.2f W/K", first, second, third)
	if first > 7 {
		t.Fatalf("first night %.2f, want near 5", first)
	}
	if second < 13 || second > 17 {
		t.Fatalf("one night after the step %.2f, want most of the way to 17 (13–17)", second)
	}
	if third < 15.5 || third > 17 {
		t.Fatalf("two nights after the step %.2f, want 15.5–17", third)
	}
	// The day between nights decays old and new alike: it moves nothing.
	before := uaOf(t, tr, right)
	tr.Tick(t0.Add(2*day + 20*time.Hour))
	if after := uaOf(t, tr, right); after.WPerK != before.WPerK || after.EvidenceHours != before.EvidenceHours {
		t.Fatalf("a day without evidence moved the fit: %+v → %+v", before, after)
	}
}

// With no room temperature there is no fit; nor with one older than
// AmbientStale.
func TestUANeedsAFreshAmbient(t *testing.T) {
	cfg := planConfig()
	s := newSynth(t)
	tr := newTracker(t, cfg)
	c := 50.0
	for at := t0; at.Before(t0.Add(4 * time.Hour)); at = at.Add(30 * time.Second) {
		c -= 0.005
		if _, err := tr.Sweep(s.sweep(left, at, c)); err != nil {
			t.Fatal(err)
		}
	}
	if u := uaOf(t, tr, left); u.EvidenceHours != 0 || u.WPerK != 4 {
		t.Fatalf("no ambient: %+v", u)
	}
	// One reading, then silence: evidence for AmbientStale after it only.
	at0 := t0.Add(4 * time.Hour)
	tr.Ambient(at0, 20)
	for at := at0; at.Before(at0.Add(2 * time.Hour)); at = at.Add(30 * time.Second) {
		c -= 0.005
		if _, err := tr.Sweep(s.sweep(left, at, c)); err != nil {
			t.Fatal(err)
		}
	}
	if u := uaOf(t, tr, left); !near(u.EvidenceHours, cfg.AmbientStale.Hours(), 0.02) {
		t.Fatalf("evidence %.3f h, want %.3f (AmbientStale)", u.EvidenceHours, cfg.AmbientStale.Hours())
	}
}

// A tank within UAMinDeltaK of the room gives no evidence.
func TestUANeedsATemperatureDifference(t *testing.T) {
	cfg := planConfig()
	s := newSynth(t)
	tr := newTracker(t, cfg)
	night(t, s, tr, left, t0, 3*time.Hour, 24.9, 20, 4)
	if u := uaOf(t, tr, left); u.EvidenceHours != 0 {
		t.Fatalf("within %v K: %+v", cfg.UAMinDeltaK, u)
	}
	tr = newTracker(t, cfg)
	night(t, s, tr, left, t0, 3*time.Hour, 25.5, 20, 4)
	if u := uaOf(t, tr, left); u.EvidenceHours == 0 {
		t.Fatalf("over %v K: no evidence", cfg.UAMinDeltaK)
	}
}

// A draw or a reheat breaks the quiet: evidence resumes UAQuiet after the
// tank is cooling again, and the draw's loss is never taken for standing
// loss.
func TestUAQuietAfterADraw(t *testing.T) {
	cfg := planConfig()
	s := newSynth(t)
	tr := newTracker(t, cfg)
	c := night(t, s, tr, left, t0, 2*time.Hour, 50, 20, 4)
	before := uaOf(t, tr, left)
	start := t0.Add(2 * time.Hour)
	tr.MeteredDraw(start, true)
	_, c = s.ramp(tr, left, start, 5*time.Minute, 10*time.Second, c, -20)
	end := start.Add(5 * time.Minute)
	tr.MeteredDraw(end, false)
	// Cooling again after the settle; the quiet runs UAQuiet from there.
	resume := end.Add(cfg.DrawSettle + time.Second)
	short := cfg.UAQuiet - time.Minute
	c = night(t, s, tr, left, resume, short, c, 20, 4)
	if u := uaOf(t, tr, left); u.EvidenceHours != before.EvidenceHours || u.WPerK != before.WPerK {
		t.Fatalf("evidence during the quiet after a draw: %+v → %+v", before, u)
	}
	night(t, s, tr, left, resume.Add(short), time.Hour, c, 20, 4)
	u := uaOf(t, tr, left)
	if !near(u.EvidenceHours-before.EvidenceHours, 1-time.Minute.Hours(), 0.02) || !near(u.WPerK, 4, 0.2) {
		t.Fatalf("after the quiet: %+v (before %+v)", u, before)
	}
}
