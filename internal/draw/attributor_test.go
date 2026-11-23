package draw

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sweeney/common/wire/boiler"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/tank"
)

// newAttributor is an attributor with HW off from before every test's
// window (as bigShower's), so a test without HW input isn't hw_unknown.
func newAttributor(t *testing.T) *Attributor {
	t.Helper()
	a, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	a.HW(t0.Add(-time.Hour), boiler.Off)
	return a
}

func started(d meter.Draw) meter.Event {
	s := d
	s.Ended, s.End, s.Litres, s.Pulses = false, d.Start, 1, 1
	return meter.Event{Type: meter.EventDrawStarted, At: d.Start, Draw: &s}
}

func ended(d meter.Draw) meter.Event {
	return meter.Event{Type: meter.EventDrawEnded, At: d.End.Add(60 * time.Second), Draw: &d}
}

// bal is the balanced attributions in rs.
func bal(rs []Result) []Attribution {
	var out []Attribution
	for _, r := range rs {
		if r.Stage == StageBalanced {
			out = append(out, r.Attribution)
		}
	}
	return out
}

// endedIDs is the draws rs ends.
func endedIDs(rs []Result) []string {
	var out []string
	for _, r := range rs {
		if r.Stage == StageEnded {
			out = append(out, r.Draw.ID)
		}
	}
	return out
}

// interleave merges the tanks' sweeps into one time-ordered feed.
func interleave(w Window) []tank.Sweep {
	var all []tank.Sweep
	for _, ss := range w.Sweeps {
		all = append(all, ss...)
	}
	slices.SortStableFunc(all, func(a, b tank.Sweep) int { return a.At.Compare(b.At) })
	return all
}

// Fed the window's sweeps and the meter's events as they arrive, the
// attributor gives the draw once, when every tank's balance sweep is in,
// and it equals Attribute over the same sweeps.
func TestAttributorEmitsOnBalanceSweep(t *testing.T) {
	a := newAttributor(t)
	d := shower()
	w := bigShower()
	var got []Attribution
	startFed, endFed := false, false
	for _, s := range interleave(w) {
		if !startFed && !s.At.Before(d.Start) {
			got = append(got, bal(a.Meter(started(d)))...)
			startFed = true
		}
		if !endFed && s.At.After(d.End.Add(60*time.Second)) {
			got = append(got, bal(a.Meter(ended(d)))...)
			endFed = true
		}
		out := bal(a.Sweep(s))
		if len(out) > 0 && s.At.Before(d.End.Add(testConfig().BalanceSettle)) {
			t.Fatalf("emitted at %v, before the balance sweep", s.At)
		}
		got = append(got, out...)
	}
	if len(got) != 1 {
		t.Fatalf("%d attributions, want 1", len(got))
	}
	want := Attribute(testConfig(), d, w)
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("attributor gave\n%+v\nwant\n%+v", got[0], want)
	}
	if a.Pending() != 0 {
		t.Errorf("%d still pending", a.Pending())
	}
}

// The programmer's HW state reaches the attribution.
func TestAttributorHWMasks(t *testing.T) {
	a := newAttributor(t)
	d := shower()
	a.HW(t0.Add(-time.Hour), boiler.Off)
	got := bal(a.Meter(started(d)))
	endFed := false
	for _, s := range interleave(bigShower()) {
		if !endFed && s.At.Equal(d.End.Add(60*time.Second)) {
			a.HW(d.End.Add(2*time.Minute), boiler.On)
			got = append(got, bal(a.Meter(ended(d)))...)
			endFed = true
		}
		got = append(got, bal(a.Sweep(s))...)
	}
	if len(got) != 1 || !got[0].MaskedByReheat || !slices.Contains(got[0].Reasons, ReasonHWOn) {
		t.Fatalf("got %+v, want one draw masked by HW", got)
	}
}

// The next draw's first litre before the balance sweep cuts the balance:
// the draw is emitted once every tank has a sweep past the cut.
func TestAttributorNextDrawCuts(t *testing.T) {
	a := newAttributor(t)
	d := shower()
	next := d
	next.ID, next.Start, next.End = "next", d.End.Add(3*time.Minute), d.End.Add(4*time.Minute)
	w := bigShower()
	for _, id := range []string{left, right} {
		ss := w.Sweeps[id]
		w.Sweeps[id] = append(ss[:4:4], sw(id, d.End.Add(2*time.Minute), ss[3].Estimate.KWh-0.1, layered(58)),
			sw(id, d.End.Add(3*time.Minute+10*time.Second), ss[3].Estimate.KWh-0.2, layered(58)), ss[4])
	}
	var got []Attribution
	got = append(got, bal(a.Meter(started(d)))...)
	endFed, nextFed := false, false
	for _, s := range interleave(w) {
		if !endFed && s.At.After(d.End.Add(60*time.Second)) {
			got = append(got, bal(a.Meter(ended(d)))...)
			endFed = true
		}
		if !nextFed && s.At.After(next.Start) {
			got = append(got, bal(a.Meter(started(next)))...)
			nextFed = true
		}
		out := bal(a.Sweep(s))
		if len(out) > 0 && len(got) == 0 && !s.At.Equal(d.End.Add(3*time.Minute+10*time.Second)) {
			t.Fatalf("emitted at %v, want at the first sweeps past the cut, not before and not at the balance target", s.At)
		}
		got = append(got, out...)
	}
	if len(got) != 1 {
		t.Fatalf("%d attributions, want 1", len(got))
	}
	if !slices.Contains(got[0].Reasons, ReasonInterrupted) {
		t.Errorf("reasons %v, want interrupted", got[0].Reasons)
	}
	if l := tankOf(t, got[0], left); !l.BalanceAt.Equal(d.End.Add(2 * time.Minute)) {
		t.Errorf("balance at %v, want End+2m (the last sweep before the cut)", l.BalanceAt)
	}
}

// A tank that stops reporting: Tick past the balance target + MaxSweepGap
// gives the draw with what there is.
func TestAttributorDeadline(t *testing.T) {
	cfg := testConfig()
	a := newAttributor(t)
	d := shower()
	w := bigShower()
	w.Sweeps[left] = w.Sweeps[left][:3] // nothing after the draw
	var got []Attribution
	got = append(got, bal(a.Meter(started(d)))...)
	for _, s := range interleave(w) {
		if s.At.After(d.End) {
			got = append(got, bal(a.Meter(ended(d)))...)
			d.End = d.End.Add(time.Hour) // feed ended once
		}
		got = append(got, bal(a.Sweep(s))...)
	}
	d = shower()
	deadline := d.End.Add(cfg.BalanceSettle + cfg.MaxSweepGap)
	if got = append(got, bal(a.Tick(deadline))...); len(got) != 0 {
		t.Fatalf("emitted at the deadline itself: %+v", got)
	}
	got = bal(a.Tick(deadline.Add(time.Second)))
	if len(got) != 1 || !slices.Equal(got[0].Reasons, []Reason{ReasonTankMissing}) {
		t.Fatalf("got %+v, want one draw with tank_missing", got)
	}
	if tankOf(t, got[0], right).LostKWh == nil {
		t.Errorf("the reporting tank's loss is missing")
	}
}

// Two draws pending at once come out in order.
func TestAttributorTwoPending(t *testing.T) {
	a := newAttributor(t)
	d1 := shower()
	d1.Litres, d1.Pulses, d1.End = 3, 3, d1.Start.Add(30*time.Second)
	d2 := d1
	d2.ID, d2.Start, d2.End = "second", d1.End.Add(2*time.Minute), d1.End.Add(2*time.Minute+30*time.Second)
	var got []Attribution
	got = append(got, bal(a.Meter(started(d1)))...)
	got = append(got, bal(a.Meter(ended(d1)))...)
	got = append(got, bal(a.Meter(started(d2)))...)
	got = append(got, bal(a.Meter(ended(d2)))...)
	for at := t0.Add(-time.Minute); at.Before(t0.Add(30 * time.Minute)); at = at.Add(30 * time.Second) {
		for _, id := range []string{left, right} {
			got = append(got, bal(a.Sweep(sw(id, at, 11-float64(at.Sub(t0))/float64(time.Hour), layered(60))))...)
		}
	}
	if len(got) != 2 || got[0].Draw.ID != d1.ID || got[1].Draw.ID != "second" {
		t.Fatalf("got %d attributions %v, want d1 then d2", len(got), got)
	}
	if !slices.Contains(got[0].Reasons, ReasonInterrupted) {
		t.Errorf("first draw's reasons %v, want interrupted by the second", got[0].Reasons)
	}
}

// A draw whose start washhouse didn't see (it started mid-draw) has no
// before sweeps: it is given once its balance sweeps are in, with every
// tank missing.
func TestAttributorEndedWithoutStarted(t *testing.T) {
	a := newAttributor(t)
	d := shower()
	var got []Attribution
	endFed := false
	for _, s := range interleave(bigShower()) {
		if s.At.Before(d.Start.Add(time.Minute)) {
			continue // washhouse wasn't running yet
		}
		if !endFed && s.At.Equal(d.End.Add(60*time.Second)) {
			got = append(got, bal(a.Meter(ended(d)))...)
			endFed = true
		}
		got = append(got, bal(a.Sweep(s))...)
	}
	if len(got) != 1 || !slices.Equal(got[0].Reasons, []Reason{ReasonTankMissing}) || got[0].Tanks[0].Share != nil {
		t.Fatalf("got %+v, want one draw with every tank missing", got)
	}
}

// Sweeps from an unconfigured tank, and a tank's sweeps out of order, are
// ignored; other meter events are no-ops.
func TestAttributorIgnores(t *testing.T) {
	a := newAttributor(t)
	if out := a.Sweep(sw("other", t0, 1, layered(50))); out != nil || a.Buffered() != 0 {
		t.Errorf("an unconfigured tank's sweep was taken")
	}
	a.Sweep(sw(left, t0, 1, layered(50)))
	a.Sweep(sw(left, t0.Add(-time.Second), 1, layered(50)))
	a.Sweep(sw(left, t0, 1, layered(50)))
	if a.Buffered() != 1 {
		t.Errorf("%d sweeps buffered, want 1", a.Buffered())
	}
	if out := a.Meter(meter.Event{Type: meter.EventLitre, At: t0, Litre: &meter.Litre{}}); out != nil {
		t.Errorf("a litre event gave %v", out)
	}
}

// With no draw active or pending, the buffer keeps only MaxSweepGap of
// sweeps (plus the one before), so it stays small over a quiet night; a
// draw after it still finds its before sweep.
func TestAttributorBufferBounded(t *testing.T) {
	cfg := testConfig()
	a := newAttributor(t)
	at := t0
	for ; at.Before(t0.Add(8 * time.Hour)); at = at.Add(30 * time.Second) {
		for _, id := range []string{left, right} {
			a.Sweep(sw(id, at, 11, layered(55)))
		}
		a.HW(at, boiler.Off)
	}
	perTank := int(cfg.MaxSweepGap/(30*time.Second)) + 2
	if n := a.Buffered(); n > 2*perTank {
		t.Errorf("%d sweeps buffered after 8 h, want at most %d", n, 2*perTank)
	}
	if n := len(a.hw); n > perTank {
		t.Errorf("%d HW states kept, want at most %d", n, perTank)
	}
	d := meter.Draw{ID: "late", Start: at.Add(5 * time.Second), End: at.Add(5*time.Minute + 5*time.Second), Litres: 30, Pulses: 30, Ended: true}
	a.Meter(started(d))
	var got []Attribution
	for ; at.Before(d.End.Add(10 * time.Minute)); at = at.Add(30 * time.Second) {
		for _, id := range []string{left, right} {
			kwh := 11.0
			if at.After(d.Start) {
				kwh = 10
			}
			got = append(got, bal(a.Sweep(sw(id, at, kwh, layered(60))))...)
		}
		if at.After(d.End.Add(time.Minute)) && at.Before(d.End.Add(90*time.Second)) {
			got = append(got, bal(a.Meter(ended(d)))...)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d attributions, want 1", len(got))
	}
	if l := tankOf(t, got[0], left); !l.Before.Equal(d.Start.Add(-5*time.Second)) || !near(l.LostKWh, 1, 1e-9) {
		t.Errorf("before %v lost %v, want the sweep 5 s before the draw and 1 kWh", l.Before, show(l.LostKWh))
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	cfg := testConfig()
	cfg.ShareSettle = 0
	if _, err := New(cfg); err == nil {
		t.Fatal("New took an invalid config")
	}
}

// A draw is ended (StageEnded, for draw_ended) once the meter has ended it
// and every tank has a sweep at or after the last litre + ShareSettle; it
// is balanced later, at the balance sweeps. The ended result carries the
// meter's draw.
func TestAttributorEndsAtTheShareSweep(t *testing.T) {
	cfg := testConfig()
	a := newAttributor(t)
	d := shower()
	w := bigShower()
	a.Meter(started(d))
	endFed := false
	var endedAt, balancedAt time.Time
	for _, s := range interleave(w) {
		var rs []Result
		if !endFed && !s.At.Before(d.End.Add(cfg.ShareSettle)) {
			e := ended(d)
			e.At = d.End.Add(cfg.ShareSettle - time.Second)
			if rs := a.Meter(e); len(rs) != 0 {
				t.Fatalf("results %+v on the meter's end, before the share sweeps", rs)
			}
			endFed = true
		}
		rs = a.Sweep(s)
		for _, r := range rs {
			switch r.Stage {
			case StageEnded:
				if !endedAt.IsZero() || r.Draw.ID != d.ID || r.Draw.Litres != d.Litres {
					t.Fatalf("ended %+v (already at %v)", r.Draw, endedAt)
				}
				endedAt = s.At
			case StageBalanced:
				balancedAt = s.At
			}
		}
	}
	if want := d.End.Add(cfg.ShareSettle); !endedAt.Equal(want) {
		t.Errorf("ended at %v, want the last tank's share sweep %v", endedAt, want)
	}
	if want := d.End.Add(cfg.BalanceSettle); !balancedAt.Equal(want) {
		t.Errorf("balanced at %v, want the balance sweep %v", balancedAt, want)
	}
}

// A tank that stops reporting: the draw is ended once event time is past
// the last litre + ShareSettle + MaxSweepGap, and not before.
func TestAttributorEndDeadline(t *testing.T) {
	cfg := testConfig()
	a := newAttributor(t)
	d := shower()
	a.Meter(started(d))
	for _, s := range interleave(bigShower()) {
		if s.At.Before(d.End) && s.DeviceID == left {
			a.Sweep(s)
		}
	}
	e := ended(d)
	a.Meter(e)
	deadline := d.End.Add(cfg.ShareSettle + cfg.MaxSweepGap)
	if rs := a.Tick(deadline); len(rs) != 0 {
		t.Fatalf("results at the deadline itself: %+v", rs)
	}
	if got := endedIDs(a.Tick(deadline.Add(time.Second))); !slices.Equal(got, []string{d.ID}) {
		t.Fatalf("ended %v past the deadline, want %s", got, d.ID)
	}
}

// The next draw starting ends every draw still waiting for its share
// sweeps, in the same call: draw_ended carries nothing the sweeps give.
func TestAttributorNextDrawEnds(t *testing.T) {
	a := newAttributor(t)
	d := shower()
	a.Meter(started(d))
	a.Meter(ended(d))
	next := d
	next.ID, next.Start = "next", d.End.Add(70*time.Second)
	if got := endedIDs(a.Meter(started(next))); !slices.Equal(got, []string{d.ID}) {
		t.Fatalf("ended %v on the next draw_started, want %s", got, d.ID)
	}
}

// Given all at once (one Tick past every deadline), a draw's end comes
// before its balance, and each comes once.
func TestAttributorEndedBeforeBalanced(t *testing.T) {
	cfg := testConfig()
	a := newAttributor(t)
	d := shower()
	a.Meter(started(d))
	a.Meter(ended(d))
	rs := a.Tick(d.End.Add(cfg.BalanceSettle + cfg.MaxSweepGap + time.Second))
	if len(rs) != 2 || rs[0].Stage != StageEnded || rs[1].Stage != StageBalanced || rs[0].Draw.ID != d.ID || rs[1].Attribution.Draw.ID != d.ID {
		t.Fatalf("results %+v, want ended then balanced", rs)
	}
	if rs := a.Tick(d.End.Add(time.Hour)); len(rs) != 0 {
		t.Errorf("results again: %+v", rs)
	}
}
