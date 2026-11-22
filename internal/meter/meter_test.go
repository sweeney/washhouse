package meter

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/wire/tanktemp"
)

func TestConfigValidate(t *testing.T) {
	if err := planConfig().Validate(); err != nil {
		t.Fatalf("plan config: %v", err)
	}
	bad := map[string]func(*Config){
		"DrawEnd zero":           func(c *Config) { c.DrawEnd = 0 },
		"DrawEnd negative":       func(c *Config) { c.DrawEnd = -time.Second },
		"BathFlow zero":          func(c *Config) { c.BathFlowLPerMin = 0 },
		"BathFlow NaN":           func(c *Config) { c.BathFlowLPerMin = math.NaN() },
		"BathFlow Inf":           func(c *Config) { c.BathFlowLPerMin = math.Inf(1) },
		"ShowerMinLitres zero":   func(c *Config) { c.ShowerMinLitres = 0 },
		"ShowerMinLitres NaN":    func(c *Config) { c.ShowerMinLitres = math.NaN() },
		"ShowerMinDuration zero": func(c *Config) { c.ShowerMinDuration = 0 },
		"CreepClosed zero":       func(c *Config) { c.CreepClosed = 0 },
		"CreepWindow zero":       func(c *Config) { c.CreepWindow = 0 },
		"StaleAfter zero":        func(c *Config) { c.StaleAfter = 0 },
		"UpdateEvery negative":   func(c *Config) { c.UpdateEvery = -time.Second },
	}
	for name, mut := range bad {
		c := planConfig()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
		if _, err := New(c); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	c := planConfig()
	c.UpdateEvery = 30 * time.Second
	if err := c.Validate(); err != nil {
		t.Errorf("UpdateEvery 30 s: %v", err)
	}
}

func TestOneDraw(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0.Add(-time.Hour))
	ev := feedAll(c, one(s.sweep(t0)))
	msgs, _ := s.run(t0.Add(5*time.Second), 10, 6*time.Second, 0.23)
	ev = append(ev, feedAll(c, msgs)...)

	started := ofType(ev, EventDrawStarted)
	if len(started) != 1 || !started[0].At.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("draw_started %+v", started)
	}
	if lit := ofType(ev, EventLitre); len(lit) != 10 || lit[9].Litre.Total != 10 || lit[0].Litre.Creep {
		t.Fatalf("litres %d, last %+v", len(lit), lit[len(lit)-1].Litre)
	}
	if len(endedDraws(ev)) != 0 {
		t.Fatal("ended before its DrawEnd")
	}
	d, ok := c.Draw()
	if !ok || d.Litres != 10 || d.Ended {
		t.Fatalf("running draw %+v %v", d, ok)
	}
	last := t0.Add(5*time.Second + 9*6*time.Second)
	if ev := c.Tick(last.Add(60*time.Second - time.Millisecond)); len(ev) != 0 {
		t.Fatalf("ended early: %+v", ev)
	}
	ev = c.Tick(last.Add(60 * time.Second))
	got := endedDraws(ev)
	if len(got) != 1 {
		t.Fatalf("ended %+v", ev)
	}
	g := got[0]
	if !ev[0].At.Equal(last.Add(60*time.Second)) || !g.Ended || g.Litres != 10 || g.Pulses != 10 ||
		!g.Start.Equal(t0.Add(5*time.Second)) || !g.End.Equal(last) || g.Duration != 54*time.Second ||
		g.FlowLPerMin == nil || *g.FlowLPerMin != 10 || g.Kind != KindBath {
		t.Errorf("draw %+v at %v", g, ev[0].At)
	}
	if g.From.Seq != 2 || g.To.Seq != 20 || !g.From.CountingSince.Equal(s.since) {
		t.Errorf("seq range %+v..%+v, want 2..20", g.From, g.To)
	}
	if _, ok := c.Draw(); ok {
		t.Error("still running")
	}
	if ev := c.Tick(last.Add(time.Hour)); len(ev) != 0 {
		t.Errorf("ended twice: %+v", ev)
	}
}

// A litre less than DrawEnd after the last joins the draw; DrawEnd or more
// after it starts a new one (the first has ended by then).
func TestDrawEndIsTheOneSegmentationRule(t *testing.T) {
	for _, tc := range []struct {
		gap   time.Duration
		draws int
	}{
		{40700 * time.Millisecond, 1}, // a real bath's pause mid-fill
		{60*time.Second - time.Millisecond, 1},
		{60 * time.Second, 2},
	} {
		c := newConsumer(t, planConfig())
		s := newSim(t0)
		ev := feedAll(c, s.litre(t0.Add(time.Second), time.Second), s.litre(t0.Add(time.Second+tc.gap), time.Second))
		ev = append(ev, c.Tick(t0.Add(time.Hour))...)
		if n := len(endedDraws(ev)); n != tc.draws {
			t.Errorf("gap %v: %d draws, want %d", tc.gap, n, tc.draws)
		}
	}
}

// A litre is a rise in count, never a change in last_pulse_at alone: live,
// tanktemp's open carried last_pulse_at 1 ms off its pulse's for one litre.
func TestLastPulseAtRoundingIsNotALitre(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	at := t0.Add(44228 * time.Millisecond)
	p := s.close(at)
	sw := s.sweep(t0.Add(45 * time.Second))
	o := s.open(at.Add(1500 * time.Millisecond))
	early := at.Add(-time.Millisecond)
	o.LastPulseAt = &early // ...44.227 on the open, ...44.228 on the pulse
	sw2 := s.sweep(t0.Add(75 * time.Second))
	ev := feedAll(c, []tanktemp.Meter{p, sw})
	// the open settles the litre, though its last_pulse_at is 1 ms off
	settled := c.Feed(o)
	lit := ofType(settled, EventLitre)
	if len(lit) != 1 || lit[0].Litre.Creep || lit[0].Litre.ClosedMS == nil || *lit[0].Litre.ClosedMS != 1500 {
		t.Fatalf("the open settled %+v", settled)
	}
	ev = append(ev, settled...)
	ev = append(ev, c.Feed(sw2)...)
	ev = append(ev, c.Tick(t0.Add(time.Hour))...)
	if lit := ofType(ev, EventLitre); len(lit) != 1 {
		t.Fatalf("litres %+v", lit)
	}
	if d := endedDraws(ev); len(d) != 1 || d[0].Litres != 1 || c.Health().Litres != 1 {
		t.Errorf("draws %+v, health %+v", d, c.Health())
	}
}

func TestKind(t *testing.T) {
	cfg := planConfig()
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		litres float64
		flow   *float64
		dur    time.Duration
		want   Kind
	}{
		{80, f(10), 300 * time.Second, KindBath},
		{3, f(10), 12 * time.Second, KindBath}, // flow alone makes a bath
		{80, f(9.999), 600 * time.Second, KindShower},
		{20, f(6), 180 * time.Second, KindShower},
		{19.9, f(6), 300 * time.Second, KindTap},
		{20, f(6), 180*time.Second - time.Millisecond, KindTap},
		{1, nil, 0, KindTap},
		{20, nil, 200 * time.Second, KindShower}, // can't happen, but no flow is not a bath
	} {
		if got := kindOf(cfg, tc.litres, tc.flow, tc.dur); got != tc.want {
			t.Errorf("%v L at %v over %v: %s, want %s", tc.litres, tc.flow, tc.dur, got, tc.want)
		}
	}
}

func TestDuplicateAndReplayAreNotNews(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	feedAll(c, one(s.sweep(t0)))
	p := s.close(t0.Add(2 * time.Second))
	o := s.open(t0.Add(3 * time.Second))
	ev := feedAll(c, []tanktemp.Meter{p, o})
	if len(ofType(ev, EventLitre)) != 1 {
		t.Fatalf("got %+v", ev)
	}
	for _, m := range []tanktemp.Meter{o, p, o} { // a retained replay, and an old one
		if ev := c.Feed(m); len(ev) != 0 {
			t.Errorf("replay of seq %d: %+v", m.Seq, ev)
		}
	}
	h := c.Health()
	if h.Duplicates != 3 || h.Messages != 6 || h.Litres != 1 {
		t.Errorf("health %+v", h)
	}
}

// The de-dup key is (counting_since, seq): the same seq in a new run is
// news, and so is a repeated timestamp with a new seq.
func TestDedupKeyIsRunAndSeq(t *testing.T) {
	c := newConsumer(t, planConfig())
	a := newSim(t0)
	feedAll(c, a.litre(t0.Add(time.Second), time.Second)) // seq 1, 2
	b := newSim(t0.Add(time.Minute))
	b.count = 0
	ev := feedAll(c, b.litre(t0.Add(2*time.Minute), time.Second)) // seq 1, 2 again, new run
	if lit := ofType(ev, EventLitre); len(lit) != 1 || lit[0].Litre.Total != 2 {
		t.Fatalf("new run's litre: %+v", ev)
	}
	// same timestamp, next seq: a sweep at the instant of the open
	sw := b.sweep(t0.Add(2*time.Minute + time.Second))
	before := c.Health().Duplicates
	c.Feed(sw)
	if c.Health().Duplicates != before {
		t.Error("a new seq with a repeated timestamp was dropped")
	}
	if h := c.Health(); h.Runs != 2 || h.Litres != 2 {
		t.Errorf("health %+v", h)
	}
}

// A counting run is told by counting_since alone: one stamped earlier than
// the last (an unsynced clock after a reboot) is still a new run.
func TestRestartWithAnEarlierCountingSince(t *testing.T) {
	c := newConsumer(t, planConfig())
	a := newSim(t0)
	feedAll(c, a.litre(t0.Add(time.Second), time.Second), one(a.sweep(t0.Add(30*time.Second))))
	b := newSim(t0.Add(-24 * time.Hour))
	ev := feedAll(c, one(b.sweep(t0.Add(5*time.Minute))), b.litre(t0.Add(6*time.Minute), time.Second))
	if lit := ofType(ev, EventLitre); len(lit) != 1 || lit[0].Litre.Total != 2 {
		t.Fatalf("got %+v", ev)
	}
	if h := c.Health(); h.Runs != 2 || !h.Run.CountingSince.Equal(b.since) {
		t.Errorf("health %+v", h)
	}
	// and the old run's messages are now stale, not news
	if ev := c.Feed(a.sweep(t0.Add(7 * time.Minute))); len(ev) != 0 || c.Health().StaleRuns != 1 {
		t.Errorf("old run: %+v, %+v", ev, c.Health())
	}
}

// Joining mid-run, the first message is the baseline: its count isn't news.
func TestJoiningMidRunIsTheBaseline(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0.Add(-time.Hour))
	for i := 0; i < 40; i++ {
		s.litre(t0.Add(time.Duration(i)*5*time.Second), time.Second)
	}
	ev := c.Feed(s.close(t0.Add(200 * time.Second))) // the retained pulse: litre 41
	if len(ev) != 0 {
		t.Fatalf("retained message made news: %+v", ev)
	}
	ev = feedAll(c, one(s.open(t0.Add(201*time.Second))), s.litre(t0.Add(205*time.Second), time.Second))
	lit := ofType(ev, EventLitre)
	if len(lit) != 1 || lit[0].Litre.Total != 1 {
		t.Fatalf("got %+v", ev)
	}
}

// A jump in count is skipped messages: the litres are counted (WIRE.md: only
// count measures litres), and the flow is per litre.
func TestCountJumpCountsSkippedLitres(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	ev := feedAll(c, s.litre(t0, time.Second), s.litre(t0.Add(6*time.Second), time.Second))
	s.litre(t0.Add(12*time.Second), time.Second) // both messages lost
	s.close(t0.Add(18 * time.Second))            // pulse lost
	ev = append(ev, feedAll(c, one(s.open(t0.Add(19*time.Second))), s.litre(t0.Add(24*time.Second), time.Second))...)
	ev = append(ev, c.Tick(t0.Add(time.Hour))...)
	d := endedDraws(ev)
	if len(d) != 1 || d[0].Litres != 5 || d[0].Pulses != 5 || *d[0].FlowLPerMin != 10 {
		t.Fatalf("draws %+v", d)
	}
	lit := ofType(ev, EventLitre)
	if len(lit) != 4 || lit[2].Litre.Skipped != 1 || lit[2].Litre.Pulses != 2 || lit[3].Litre.Total != 5 {
		t.Errorf("litres %+v", lit)
	}
	if h := c.Health(); h.CountJumps != 1 || h.Skipped != 1 || h.MissingSeq != 3 {
		t.Errorf("health %+v", h)
	}
}

func TestRestartAddsLitresAcrossRuns(t *testing.T) {
	c := newConsumer(t, planConfig())
	a := newSim(t0)
	m1, _ := a.run(t0.Add(time.Second), 3, 6*time.Second, 0.2)
	m1 = append(m1, a.sweep(t0.Add(time.Minute)))
	b := newSim(t0.Add(5 * time.Minute))
	m2 := one(b.sweep(t0.Add(5 * time.Minute)))
	more, _ := b.run(t0.Add(6*time.Minute), 4, 6*time.Second, 0.2)
	ev := feedAll(c, m1, m2, more)
	ev = append(ev, c.Tick(t0.Add(time.Hour))...)
	d := endedDraws(ev)
	if len(d) != 2 || d[0].Litres != 3 || d[1].Litres != 4 || c.Health().Litres != 7 || c.Health().Runs != 2 {
		t.Fatalf("draws %+v health %+v", d, c.Health())
	}
}

// A creep litre: the reed stayed closed for minutes, with no draw running.
func TestCreepLitreIsNeverADraw(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	ev := feedAll(c, s.litre(t0.Add(time.Second), 5*time.Minute))
	ev = append(ev, c.Tick(t0.Add(time.Hour))...)
	lit := ofType(ev, EventLitre)
	if len(lit) != 1 || !lit[0].Litre.Creep || lit[0].Litre.DrawID != 0 || *lit[0].Litre.ClosedMS != 300000 {
		t.Fatalf("got %+v", ev)
	}
	if len(ofType(ev, EventDrawStarted))+len(ofType(ev, EventDrawEnded)) != 0 {
		t.Errorf("a creep litre made a draw: %+v", ev)
	}
}

func TestCreepThreshold(t *testing.T) {
	for _, tc := range []struct {
		closed time.Duration
		creep  bool
	}{
		{10*time.Second - time.Millisecond, false},
		{10 * time.Second, true},
	} {
		c := newConsumer(t, planConfig())
		s := newSim(t0)
		ev := feedAll(c, s.litre(t0.Add(time.Second), tc.closed))
		ev = append(ev, c.Tick(t0.Add(time.Hour))...)
		lit := ofType(ev, EventLitre)
		if len(lit) != 1 || lit[0].Litre.Creep != tc.creep {
			t.Errorf("closed %v: %+v, want creep %v", tc.closed, ev, tc.creep)
		}
		if got := len(ofType(ev, EventDrawStarted)) == 1; got == tc.creep {
			t.Errorf("closed %v: draw started %v", tc.closed, got)
		}
	}
}

// A sweep showing the reed still closed CreepClosed after the litre makes
// it long; with no litre within DrawEnd it is creep, before the open comes.
func TestCreepSettledByAClosedSweep(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	ev := feedAll(c, one(s.close(t0.Add(time.Second))), one(s.sweep(t0.Add(5*time.Second))), one(s.sweep(t0.Add(11*time.Second))))
	if len(ofType(ev, EventLitre)) != 0 {
		t.Fatalf("decided too soon: %+v", ev)
	}
	ev = c.Feed(s.sweep(t0.Add(61 * time.Second)))
	if lit := ofType(ev, EventLitre); len(lit) != 1 || !lit[0].Litre.Creep || !lit[0].At.Equal(t0.Add(time.Second)) {
		t.Fatalf("got %+v", ev)
	}
	if ev := c.Feed(s.open(t0.Add(4 * time.Minute))); len(ev) != 0 {
		t.Errorf("decided twice: %+v", ev)
	}
}

// A reed closed for DrawEnd or more can't be followed in time: creep at
// once, on its open.
func TestVeryLongClosedIsCreepAtOnce(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	ev := feedAll(c, s.litre(t0, 60*time.Second))
	if lit := ofType(ev, EventLitre); len(lit) != 1 || !lit[0].Litre.Creep {
		t.Fatalf("got %+v", ev)
	}
}

// Live: a slow-opening tap held a draw's first litre closed 13.7 s, and the
// next came 23.3 s after it. A litre within DrawEnd makes the long-closed
// one the draw's first, not creep.
func TestSlowStartIsTheDrawsFirstLitre(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	ev := feedAll(c, s.litre(t0, 13690*time.Millisecond))
	at := t0.Add(23310 * time.Millisecond)
	more, _ := s.run(at, 4, 15*time.Second, 0.23)
	ev = append(ev, feedAll(c, more)...)
	ev = append(ev, c.Tick(t0.Add(time.Hour))...)
	d := endedDraws(ev)
	if len(d) != 1 || d[0].Litres != 5 || !d[0].Start.Equal(t0) {
		t.Fatalf("draws %+v", d)
	}
	for _, l := range ofType(ev, EventLitre) {
		if l.Litre.Creep {
			t.Errorf("creep litre %+v", l.Litre)
		}
	}
	if st := ofType(ev, EventDrawStarted); len(st) != 1 || !st[0].At.Equal(t0) {
		t.Errorf("started %+v", st)
	}
}

// A long-closed litre followed only after DrawEnd is creep, and the later
// litre starts its own draw.
func TestLongClosedThenALateLitre(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	ev := feedAll(c, s.litre(t0, 20*time.Second), s.litre(t0.Add(61*time.Second), time.Second))
	ev = append(ev, c.Tick(t0.Add(time.Hour))...)
	lit := ofType(ev, EventLitre)
	if len(lit) != 2 || !lit[0].Litre.Creep || lit[1].Litre.Creep || len(endedDraws(ev)) != 1 || endedDraws(ev)[0].Litres != 1 {
		t.Fatalf("got %+v", ev)
	}
}

// The next pulse settles a litre whose open was lost: the reed must have
// opened before it closed again.
func TestNextPulseSettlesALostOpen(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	p1 := s.close(t0.Add(time.Second))
	s.open(t0.Add(2 * time.Second)) // lost
	ev := feedAll(c, []tanktemp.Meter{p1, s.close(t0.Add(7 * time.Second))})
	lit := ofType(ev, EventLitre)
	if len(ofType(ev, EventDrawStarted)) != 1 || len(lit) != 2 || lit[0].Litre.Creep || *lit[0].Litre.ClosedMS != 1000 {
		t.Fatalf("got %+v", ev)
	}
	if ev := c.Feed(s.open(t0.Add(8 * time.Second))); len(ev) != 0 {
		t.Fatalf("got %+v", ev)
	}
	if d, _ := c.Draw(); d.Litres != 2 {
		t.Errorf("draw %+v", d)
	}
}

// A long-closed litre inside a running draw (the pointer stopping on the
// magnet at the end of a shower) is part of the draw, not creep.
func TestLongClosedLitreInADrawIsTheDraw(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	msgs, at := s.run(t0, 5, 8*time.Second, 0.25)
	ev := feedAll(c, msgs, s.litre(at, 3*time.Hour))
	ev = append(ev, c.Tick(at.Add(4*time.Hour))...)
	d := endedDraws(ev)
	if len(d) != 1 || d[0].Litres != 6 {
		t.Fatalf("draws %+v", d)
	}
	for _, l := range ofType(ev, EventLitre) {
		if l.Litre.Creep {
			t.Errorf("creep litre %+v", l.Litre)
		}
	}
}

func TestContinuousFlow(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	creep := func(at time.Time) []Event { return feedAll(c, s.litre(at, 5*time.Minute)) }
	ev := creep(t0)
	if len(ofType(ev, EventContinuousFlowStarted)) != 0 {
		t.Fatal("one creep litre started a flow")
	}
	ev = creep(t0.Add(14 * time.Minute))
	st := ofType(ev, EventContinuousFlowStarted)
	if len(st) != 1 {
		t.Fatalf("got %+v", ev)
	}
	f := st[0].Flow
	if !f.Since.Equal(t0) || !st[0].At.Equal(t0.Add(14*time.Minute)) || f.Pulses != 2 || f.Litres != 2 ||
		math.Abs(f.FlowLPerMin-1.0/14) > 1e-9 {
		t.Errorf("flow %+v at %v", f, st[0].At)
	}
	ev = creep(t0.Add(28 * time.Minute))
	if len(ofType(ev, EventContinuousFlowStarted)) != 0 {
		t.Error("started twice")
	}
	// a real draw meanwhile is a draw, and doesn't end the flow
	msgs, _ := s.run(t0.Add(35*time.Minute), 4, 10*time.Second, 0.25)
	ev = feedAll(c, msgs)
	if len(ofType(ev, EventDrawStarted)) != 1 {
		t.Errorf("draw during a flow: %+v", ev)
	}
	last := t0.Add(28 * time.Minute)
	if ev := c.Tick(last.Add(2700*time.Second - time.Millisecond)); len(ofType(ev, EventContinuousFlowEnded)) != 0 {
		t.Fatalf("ended early: %+v", ev)
	}
	if fl, ok := c.Flow(); !ok || fl.Pulses != 3 {
		t.Errorf("running flow %+v %v", fl, ok)
	}
	ev = c.Tick(last.Add(2700 * time.Second))
	en := ofType(ev, EventContinuousFlowEnded)
	if len(en) != 1 || !en[0].At.Equal(last.Add(2700*time.Second)) || !en[0].Flow.Ended || en[0].Flow.Pulses != 3 ||
		!en[0].Flow.Last.Equal(last) {
		t.Fatalf("got %+v", ev)
	}
	if _, ok := c.Flow(); ok {
		t.Error("still running")
	}
}

func TestCreepWindow(t *testing.T) {
	for _, tc := range []struct {
		apart time.Duration
		flow  bool
	}{
		{2700 * time.Second, true},
		{2700*time.Second + time.Millisecond, false},
	} {
		c := newConsumer(t, planConfig())
		s := newSim(t0)
		ev := feedAll(c, s.litre(t0, time.Minute), s.litre(t0.Add(tc.apart), time.Minute))
		if got := len(ofType(ev, EventContinuousFlowStarted)) == 1; got != tc.flow {
			t.Errorf("creep litres %v apart: flow %v, want %v", tc.apart, got, tc.flow)
		}
	}
}

func TestDrawUpdatedHook(t *testing.T) {
	cfg := planConfig()
	c := newConsumer(t, cfg)
	s := newSim(t0)
	msgs, _ := s.run(t0, 20, 6*time.Second, 0.23)
	if n := len(ofType(feedAll(c, msgs), EventDrawUpdated)); n != 0 {
		t.Errorf("UpdateEvery 0: %d updates", n)
	}
	cfg.UpdateEvery = 30 * time.Second
	c = newConsumer(t, cfg)
	s = newSim(t0)
	msgs, _ = s.run(t0, 20, 6*time.Second, 0.23) // litres at 0, 6, ... 114 s
	up := ofType(feedAll(c, msgs), EventDrawUpdated)
	if len(up) != 3 || !up[0].At.Equal(t0.Add(30*time.Second)) || up[0].Draw.Litres != 6 || up[2].Draw.Litres != 16 {
		t.Errorf("updates %d: %+v", len(up), up)
	}
}

func TestHealth(t *testing.T) {
	c := newConsumer(t, planConfig())
	if c.Fresh(t0) {
		t.Error("fresh with no message")
	}
	s := newSim(t0)
	s.lost = 2
	m := s.sweep(t0)
	m.Noise, m.Short, m.Glitches = 6, 1, 3
	m.ClockSynced = false
	c.Feed(m)
	h := c.Health()
	if h.Noise != 6 || h.Short != 1 || h.Glitches != 3 || h.Lost != 2 || h.LostBoundLitres != 4 ||
		h.ClockSynced || h.Level != tanktemp.LevelOpen || !h.LastTimestamp.Equal(t0) || h.Run.Seq != 1 || h.Runs != 1 {
		t.Errorf("health %+v", h)
	}
	if !c.Fresh(t0.Add(90*time.Second)) || c.Fresh(t0.Add(90*time.Second+time.Millisecond)) {
		t.Error("freshness boundary")
	}
	s.seq += 4 // four messages lost in transit
	c.Feed(s.sweep(t0.Add(30 * time.Second)))
	if h := c.Health(); h.MissingSeq != 4 || h.Run.Seq != 6 {
		t.Errorf("health %+v", h)
	}
	s.count = 0
	bad := s.sweep(t0.Add(time.Minute))
	bad.Count = 0
	s.count = 3
	c.Feed(s.sweep(t0.Add(70 * time.Second)))
	c.Feed(bad) // older seq: a duplicate, not a fall
	fell := s.sweep(t0.Add(80 * time.Second))
	fell.Count = 1
	c.Feed(fell)
	if h := c.Health(); h.CountFell != 1 {
		t.Errorf("count fell not seen: %+v", h)
	}
}

// Event time only moves forward, and only from messages and Tick.
func TestTickNeverGoesBack(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	feedAll(c, s.litre(t0, time.Second))
	c.Tick(t0.Add(30 * time.Second))
	if ev := c.Tick(t0); len(ev) != 0 {
		t.Errorf("%+v", ev)
	}
	if ev := c.Tick(t0.Add(60 * time.Second)); len(endedDraws(ev)) != 1 {
		t.Errorf("%+v", ev)
	}
}

// The core is pure: no wall clock in the package (boundaries_test.go checks
// its imports).
func TestNoWallClock(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"time.Now", "time.Since", "time.Until", "time.Sleep", "time.After(", "time.Tick", "time.NewTimer", "go func"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s uses %s", e.Name(), bad)
			}
		}
	}
}
