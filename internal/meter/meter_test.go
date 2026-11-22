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
		"DeviceID empty":         func(c *Config) { c.DeviceID = "" },
		"DrawEnd zero":           func(c *Config) { c.DrawEnd = 0 },
		"DrawEnd negative":       func(c *Config) { c.DrawEnd = -time.Second },
		"BathFlow zero":          func(c *Config) { c.BathFlowLPerMin = 0 },
		"BathFlow NaN":           func(c *Config) { c.BathFlowLPerMin = math.NaN() },
		"BathFlow Inf":           func(c *Config) { c.BathFlowLPerMin = math.Inf(1) },
		"ShowerMinLitres zero":   func(c *Config) { c.ShowerMinLitres = 0 },
		"ShowerMinLitres NaN":    func(c *Config) { c.ShowerMinLitres = math.NaN() },
		"ShowerMinDuration zero": func(c *Config) { c.ShowerMinDuration = 0 },
		"ShowerMinFlow zero":     func(c *Config) { c.ShowerMinFlowLPerMin = 0 },
		"ShowerMinFlow NaN":      func(c *Config) { c.ShowerMinFlowLPerMin = math.NaN() },
		"ShowerMinFlow Inf":      func(c *Config) { c.ShowerMinFlowLPerMin = math.Inf(1) },
		"BathMinLitres zero":     func(c *Config) { c.BathMinLitres = 0 },
		"BathMinLitres negative": func(c *Config) { c.BathMinLitres = -1 },
		"BathMinLitres NaN":      func(c *Config) { c.BathMinLitres = math.NaN() },
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

// tanktemp's meters are a list: a second meter's messages are refused and
// counted, never merged into the configured meter's runs and litres.
func TestOtherDevicesAreRefused(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	feedAll(c, one(s.sweep(t0)))
	o := newSim(t0.Add(time.Minute)) // the other meter: its own run
	msgs, _ := o.run(t0.Add(10*time.Second), 5, 6*time.Second, 0.23)
	for i := range msgs {
		msgs[i].DeviceID = "cold_water_meter"
	}
	ev := feedAll(c, msgs)
	ev = append(ev, c.Tick(t0.Add(time.Hour))...)
	if len(ev) != 0 {
		t.Fatalf("the other meter made news: %+v", ev)
	}
	h := c.Health()
	if h.OtherDevices != 10 || h.Messages != 11 || h.Runs != 1 || h.Litres != 0 || h.Run.Seq != 1 ||
		!h.Run.CountingSince.Equal(s.since) {
		t.Fatalf("health %+v", h)
	}
	ev = feedAll(c, s.litre(t0.Add(2*time.Hour), time.Second))
	if lit := ofType(ev, EventLitre); len(lit) != 1 || lit[0].Litre.Total != 1 {
		t.Errorf("the configured meter after: %+v", ev)
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
		g.FlowLPerMin == nil || *g.FlowLPerMin != 10 || g.Kind != KindTap { // 10 L: too few for a bath
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
		{20, f(10), 120 * time.Second, KindBath},
		{19.9, f(10), 120 * time.Second, KindTap}, // full flow, but too few litres for a bath
		{3, f(10), 12 * time.Second, KindTap},     // a short full-flow hot tap
		{80, f(9.999), 600 * time.Second, KindShower},
		{20, f(6), 180 * time.Second, KindShower},
		{19.9, f(6), 300 * time.Second, KindTap},
		{20, f(6), 180*time.Second - time.Millisecond, KindTap},
		{60, f(3), 1200 * time.Second, KindShower},
		{60, f(2.999), 1200 * time.Second, KindTap}, // a dribble, not a shower
		{1, nil, 0, KindTap},
		{20, nil, 200 * time.Second, KindTap}, // can't happen: no flow is neither a bath nor a shower
	} {
		if got := kindOf(cfg, tc.litres, tc.flow, tc.dur); got != tc.want {
			t.Errorf("%v L at %v over %v: %s, want %s", tc.litres, tc.flow, tc.dur, got, tc.want)
		}
	}
}

// What a steady flow comes out as, across the range from a dribble to a
// bath: 60 litres at duty 0.24 (the creep/draw split falls at DrawEnd, so a
// steady flow with litres under 60 s apart is a draw, however slow), and a
// short full-flow hot tap. Slow steady draws are taps, not showers
// (ShowerMinFlowLPerMin); they are not folded into continuous flow.
func TestSteadyFlowOutcomes(t *testing.T) {
	type outcome struct {
		kind  Kind // "" for no draw
		creep bool // every litre creep, in one continuous flow
	}
	for _, tc := range []struct {
		lpm    float64
		litres int
		want   outcome
	}{
		{0.8, 60, outcome{creep: true}},
		{1.0, 60, outcome{creep: true}}, // litres exactly DrawEnd apart
		{1.2, 60, outcome{kind: KindTap}},
		{1.5, 60, outcome{kind: KindTap}},
		{2.0, 60, outcome{kind: KindTap}},
		{3, 60, outcome{kind: KindShower}},
		{4.9, 60, outcome{kind: KindShower}},
		{6.3, 60, outcome{kind: KindShower}},
		{15, 60, outcome{kind: KindBath}},
		{12, 3, outcome{kind: KindTap}}, // a short full-flow hot tap
	} {
		c := newConsumer(t, planConfig())
		s := newSim(t0)
		iv := time.Duration(float64(time.Minute) / tc.lpm)
		start := one(s.sweep(t0))
		msgs, end := s.run(t0.Add(time.Second), tc.litres, iv, 0.24)
		ev := feedAll(c, start, msgs)
		ev = append(ev, c.Tick(end.Add(24*time.Hour))...)
		var got outcome
		d := endedDraws(ev)
		lit := ofType(ev, EventLitre)
		creep := 0
		for _, l := range lit {
			if l.Litre.Creep {
				creep++
			}
		}
		switch {
		case len(d) == 1 && d[0].Pulses == tc.litres && creep == 0:
			got.kind = d[0].Kind
		case len(d) == 0 && creep == tc.litres && len(ofType(ev, EventContinuousFlowStarted)) == 1 &&
			len(ofType(ev, EventContinuousFlowEnded)) == 1:
			got.creep = true
		default:
			t.Errorf("%v L/min: %d draws %+v, %d creep litres of %d", tc.lpm, len(d), d, creep, len(lit))
			continue
		}
		if got != tc.want {
			t.Errorf("%v L/min, %d L: %+v, want %+v", tc.lpm, tc.litres, got, tc.want)
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

// Joining mid-run on a cold start (no run seen yet), the first message is
// the baseline: its count isn't news.
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

// A restart this consumer lived through, first seen mid-run (the broker or
// washhouse's connection dropped across it): the baseline rule is for a
// cold start only, so everything the new run counted is new. Count starts
// at 0 per run (WIRE.md), so it all arrives as one count jump: the litre
// at last_pulse_at, timed, and the rest untimed.
func TestRestartSeenMidRunCountsTheNewRun(t *testing.T) {
	c := newConsumer(t, planConfig())
	a := newSim(t0)
	feedAll(c, one(a.sweep(t0)))
	b := newSim(t0.Add(time.Minute))
	b.sweep(t0.Add(time.Minute))                                    // seq 1: lost
	_, end := b.run(t0.Add(70*time.Second), 5, 6*time.Second, 0.23) // seq 2-11: lost
	ev := c.Feed(b.sweep(t0.Add(2 * time.Minute)))                  // seq 12, count 5
	ev = append(ev, c.Tick(end.Add(time.Hour))...)
	h := c.Health()
	if h.Litres != 5 || h.Runs != 2 || h.MissingSeq != 11 || h.CountJumps != 1 || h.Skipped != 4 {
		t.Fatalf("health %+v", h)
	}
	lit := ofType(ev, EventLitre)
	last := t0.Add(70*time.Second + 4*6*time.Second)
	if len(lit) != 1 || lit[0].Litre.Pulses != 5 || lit[0].Litre.Skipped != 4 || !lit[0].At.Equal(last) ||
		lit[0].Litre.Total != 5 {
		t.Fatalf("litres %+v", lit)
	}
	if d := endedDraws(ev); len(d) != 1 || d[0].Litres != 5 {
		t.Errorf("draws %+v", d)
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

// A tanktemp restart with a litre still waiting (the usual deploy case: a
// restart during a draw): the new run's first message settles it, a draw's
// first litre, since count is the truth for volume. It isn't carried into
// the new run, where nothing would ever settle it.
func TestRestartSettlesTheWaitingLitre(t *testing.T) {
	c := newConsumer(t, planConfig())
	a := newSim(t0)
	ev := feedAll(c, one(a.sweep(t0)), one(a.close(t0.Add(5*time.Second)))) // its open never comes
	if len(ofType(ev, EventLitre)) != 0 {
		t.Fatalf("decided too soon: %+v", ev)
	}
	b := newSim(t0.Add(8 * time.Second))
	ev = c.Feed(b.sweep(t0.Add(8 * time.Second)))
	lit := ofType(ev, EventLitre)
	if len(ofType(ev, EventDrawStarted)) != 1 || len(lit) != 1 || lit[0].Litre.Creep ||
		lit[0].Litre.Ref.CountingSince != a.since || !lit[0].At.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("the new run's first message: %+v", ev)
	}
	ev = feedAll(c, one(b.sweep(t0.Add(38*time.Second))), one(b.sweep(t0.Add(68*time.Second))))
	if d := endedDraws(ev); len(d) != 1 || d[0].Litres != 1 || !d[0].End.Equal(t0.Add(5*time.Second)) {
		t.Errorf("draws %+v", d)
	}
}

// After a reboot the Pi has no RTC and runs on the clock saved at
// shutdown until NTP syncs: tanktemp stamps clock_synced false, with times
// that can be earlier than the last run's. Those litres are counted (health
// and Total) but are never timed: not a litre event, not in a draw, and not
// event time. Once synced, litres are timed again.
func TestUnsyncedClockIsCountedNotTimed(t *testing.T) {
	c := newConsumer(t, planConfig())
	a := newSim(t0)
	start := one(a.sweep(t0))
	msgs, end := a.run(t0.Add(time.Second), 5, 6*time.Second, 0.23)
	feedAll(c, start, msgs) // a draw, still running when tanktemp stops
	old := t0.Add(-24 * time.Hour)
	b := newSim(old)
	b.unsynced = true
	bstart := one(b.sweep(old))
	bmsgs, _ := b.run(old.Add(10*time.Second), 3, 6*time.Second, 0.23)
	ev := feedAll(c, bstart, bmsgs)
	if len(ev) != 0 {
		t.Fatalf("unsynced litres made news: %+v", ev)
	}
	if d, ok := c.Draw(); !ok || d.Litres != 5 {
		t.Fatalf("the running draw %+v %v", d, ok)
	}
	if h := c.Health(); h.Litres != 8 || h.Unsynced != 3 || h.Unattributed != 0 || h.ClockSynced {
		t.Fatalf("health %+v", h)
	}
	b.unsynced = false // NTP synced: the clock steps to real time
	real := end.Add(5 * time.Minute)
	ev = feedAll(c, one(b.sweep(real)))
	if d := endedDraws(ev); len(d) != 1 || d[0].Litres != 5 || !ev[0].At.Equal(end.Add(-6*time.Second+time.Minute)) {
		t.Fatalf("the old run's draw: %+v", ev)
	}
	more, _ := b.run(real.Add(10*time.Second), 4, 6*time.Second, 0.23)
	ev = append(feedAll(c, more), c.Tick(real.Add(time.Hour))...)
	d := endedDraws(ev)
	if len(d) != 1 || d[0].Litres != 4 || !d[0].Start.Equal(real.Add(10*time.Second)) {
		t.Fatalf("draws after the sync %+v", d)
	}
	if lit := ofType(ev, EventLitre); len(lit) != 4 || lit[3].Litre.Total != 12 {
		t.Errorf("litres %+v", lit)
	}
}

// A cold start on an unsynced message mid-run takes its count as the
// baseline but not its last_pulse_at: here the clock was an hour ahead, and
// taking it would leave every synced litre after it untimed.
func TestUnsyncedBaselineTimesNothing(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0.Add(-time.Hour))
	s.unsynced = true
	s.litre(t0.Add(time.Hour), time.Second) // seq 1, 2: before this consumer started
	ev := c.Feed(s.sweep(t0.Add(time.Hour + 30*time.Second)))
	s.unsynced = false // NTP synced: the clock steps back to real time
	msgs, end := s.run(t0.Add(time.Minute), 3, 6*time.Second, 0.23)
	ev = append(ev, feedAll(c, msgs)...)
	ev = append(ev, c.Tick(end.Add(time.Hour))...)
	if d := endedDraws(ev); len(d) != 1 || d[0].Litres != 3 || len(ofType(ev, EventLitre)) != 3 {
		t.Fatalf("got %+v", ev)
	}
	if h := c.Health(); h.Litres != 3 || h.Unattributed != 0 || h.Unsynced != 0 {
		t.Errorf("health %+v", h)
	}
}

// A litre with no usable time (here the synced clock stepped back 7 s, and
// last_pulse_at moved with it, so it is no later than the last litre's)
// joins the running draw untimed: counted in the draw, its flow untouched.
func TestUntimedLitreJoinsTheRunningDraw(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	msgs, at := s.run(t0, 3, 6*time.Second, 0.23) // litres at 0, 6, 12 s
	feedAll(c, msgs)
	m := s.close(at.Add(-7 * time.Second)) // really at 18 s: stamped 11 s
	ev := c.Feed(m)
	d, ok := c.Draw()
	lit := ofType(ev, EventLitre)
	if !ok || d.Litres != 4 || d.Pulses != 4 || d.To.Seq != m.Seq || *d.FlowLPerMin != 10 ||
		len(lit) != 1 || lit[0].Litre.Skipped != 1 || lit[0].Litre.DrawID != d.ID || lit[0].Litre.Total != 4 {
		t.Fatalf("draw %+v %v, events %+v", d, ok, ev)
	}
	if h := c.Health(); h.Unattributed != 0 || h.Skipped != 1 || h.Litres != 4 {
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
	if len(lit) != 1 || !lit[0].Litre.Creep || lit[0].Litre.DrawID != "" || *lit[0].Litre.ClosedMS != 300000 {
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
// once, on the message that shows it (its open, or a sweep after a lost
// open), with no Tick needed.
func TestVeryLongClosedIsCreepAtOnce(t *testing.T) {
	for _, lostOpen := range []bool{false, true} {
		c := newConsumer(t, planConfig())
		s := newSim(t0)
		if ev := c.Feed(s.close(t0)); len(ev) != 0 {
			t.Fatalf("decided on the pulse: %+v", ev)
		}
		m := s.open(t0.Add(60 * time.Second))
		if lostOpen {
			m = s.sweep(t0.Add(61 * time.Second))
		}
		ev := c.Feed(m)
		if lit := ofType(ev, EventLitre); len(lit) != 1 || !lit[0].Litre.Creep || !lit[0].At.Equal(t0) {
			t.Errorf("lost open %v: %+v", lostOpen, ev)
		}
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

// A pulse's last_closed_ms is the litre before it's: the waiting litre's
// only when no pulse was skipped between them. After a count jump it is a
// skipped litre's, so the waiting litre's closed time stays unknown.
func TestCountJumpLastClosedIsNotTheWaitingLitres(t *testing.T) {
	c := newConsumer(t, planConfig())
	s := newSim(t0)
	p1 := s.close(t0)                            // the waiting litre: closed 20 s ...
	s.open(t0.Add(20 * time.Second))             // ... but its open is lost
	s.litre(t0.Add(25*time.Second), time.Second) // a litre closed 1 s: both lost
	p3 := s.close(t0.Add(30 * time.Second))      // count +2, last_closed_ms 1000
	if *p3.LastClosedMS != 1000 || p3.Count != 3 {
		t.Fatalf("sim: %+v", p3)
	}
	ev := feedAll(c, []tanktemp.Meter{p1, p3})
	lit := ofType(ev, EventLitre)
	if len(lit) != 2 || lit[0].Litre.Creep || lit[1].Litre.Pulses != 2 {
		t.Fatalf("litres %+v", ev)
	}
	if cl := lit[0].Litre.ClosedMS; cl != nil {
		t.Errorf("the waiting litre took the skipped litre's closed time, %d ms", *cl)
	}
}

// The next pulse is the first word on a waiting litre whose open was lost,
// and its last_closed_ms says the waiting litre was long. Exactly DrawEnd
// later it was not followed in time: creep. A millisecond sooner it was:
// the draw's first litre.
func TestLongClosedFollowedAtExactlyDrawEnd(t *testing.T) {
	for _, tc := range []struct {
		gap   time.Duration
		creep bool
	}{
		{60*time.Second - time.Millisecond, false},
		{60 * time.Second, true},
	} {
		c := newConsumer(t, planConfig())
		s := newSim(t0)
		p1 := s.close(t0.Add(time.Second))
		s.open(t0.Add(21 * time.Second)) // lost
		p2 := s.close(t0.Add(time.Second + tc.gap))
		ev := feedAll(c, []tanktemp.Meter{p1, p2})
		lit := ofType(ev, EventLitre)
		if len(lit) == 0 || lit[0].Litre.Creep != tc.creep || lit[0].Litre.ClosedMS == nil || *lit[0].Litre.ClosedMS != 20000 {
			t.Errorf("gap %v: %+v, want creep %v", tc.gap, ev, tc.creep)
		}
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

// A draw's ID comes from its first litre's message, (counting_since, seq),
// not from a per-process counter: statehouse keys a signal and an activity
// on it, and washhouse restarting must not reuse an earlier draw's ID.
func TestDrawIDIsItsFirstLitre(t *testing.T) {
	msgs := func() []tanktemp.Meter {
		a := newSim(t0)
		start := one(a.sweep(t0))
		m1, _ := a.run(t0.Add(time.Second), 3, 6*time.Second, 0.2)
		m2, _ := a.run(t0.Add(5*time.Minute), 2, 6*time.Second, 0.2)
		return append(append(start, m1...), m2...)
	}
	draws := func() []Draw {
		c := newConsumer(t, planConfig())
		ev := feedAll(c, msgs())
		return endedDraws(append(ev, c.Tick(t0.Add(time.Hour))...))
	}
	first, again := draws(), draws()
	if len(first) != 2 {
		t.Fatalf("draws %+v", first)
	}
	if first[0].ID == first[1].ID {
		t.Errorf("two draws share ID %q", first[0].ID)
	}
	for i, d := range first {
		if want := DrawID(d.From); d.ID != want {
			t.Errorf("draw %d ID %q, want %q from its first litre %+v", i, d.ID, want, d.From)
		}
		if again[i].ID != d.ID {
			t.Errorf("draw %d: ID %q, then %q from a fresh consumer on the same messages", i, d.ID, again[i].ID)
		}
	}
	// Same seq in a later counting run is a different draw.
	if DrawID(Ref{CountingSince: t0, Seq: 2}) == DrawID(Ref{CountingSince: t0.Add(time.Hour), Seq: 2}) {
		t.Error("the ID ignores counting_since")
	}
	if got := DrawID(Ref{CountingSince: t0, Seq: 2}); got != "20010101T070000Z-2" {
		t.Errorf("DrawID = %q", got)
	}
}
