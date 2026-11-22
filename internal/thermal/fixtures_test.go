package thermal

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/replay"
)

// The recorded fixtures (testdata/fixtures, README.md there) replayed
// through internal/tank, internal/meter and the tracker, as the engine will
// wire them: tanktemp states become sweeps; the meter consumer's
// draw_started and draw_ended become MeteredDraw; programmer events and
// heartbeats (never a RECONNECTED or a last will, which carry no switch
// states) become Programmer; the boiler-room climate becomes Ambient.

const (
	fixtureDir   = "../../testdata/fixtures"
	ambientTopic = "zigbee2mqtt/climate_groundfloor_boiler-room"
)

// snap is a tank's status just after one of its sweeps.
type snap struct {
	at     time.Time
	state  State
	source Source
	power  *float64
}

type run struct {
	snaps    map[string][]snap
	events   []Event
	draws    []meter.Draw // ended draws, in fixture time
	ambients int
	end      time.Time
}

// feed replays a fixture into tr, every time moved by shift (so fixtures,
// which all start at the same fake epoch, can follow one another).
func feed(t *testing.T, tr *Tracker, name string, shift time.Duration) run {
	t.Helper()
	msgs, err := replay.ReadFile(filepath.Join(fixtureDir, name+".log.gz"), replay.Options{Ambient: ambientTopic})
	if err != nil {
		t.Fatal(err)
	}
	tanks := houseTankers(t)
	mc, err := meter.New(meterConfig())
	if err != nil {
		t.Fatal(err)
	}
	r := run{snaps: map[string][]snap{}}
	onMeter := func(evs []meter.Event) {
		for _, e := range evs {
			switch e.Type {
			case meter.EventDrawStarted:
				r.events = append(r.events, tr.MeteredDraw(e.At.Add(shift), true)...)
			case meter.EventDrawEnded:
				r.events = append(r.events, tr.MeteredDraw(e.At.Add(shift), false)...)
				r.draws = append(r.draws, *e.Draw)
			}
		}
	}
	for _, m := range msgs {
		switch {
		case m.State != nil:
			sw, err := tanks[m.State.DeviceID].Sweep(*m.State)
			if err != nil {
				t.Fatal(err)
			}
			onMeter(mc.Tick(sw.At))
			sw.At = sw.At.Add(shift)
			ev, err := tr.Sweep(sw)
			if err != nil {
				t.Fatal(err)
			}
			r.events = append(r.events, ev...)
			st := mustStatus(t, tr, sw.DeviceID)
			r.snaps[sw.DeviceID] = append(r.snaps[sw.DeviceID], snap{at: sw.At, state: st.State, source: st.Source, power: st.PowerKW})
			if sw.At.After(r.end) {
				r.end = sw.At
			}
		case m.Meter != nil:
			onMeter(mc.Feed(*m.Meter))
		case m.BoilerEvent != nil:
			e := m.BoilerEvent
			r.events = append(r.events, tr.Programmer(e.Timestamp.Add(shift), e.HW, e.CH)...)
		case m.BoilerSystem != nil:
			s := m.BoilerSystem
			if s.IsHeartbeat() && (s.Ready == nil || *s.Ready) {
				r.events = append(r.events, tr.Programmer(s.Timestamp.Add(shift), s.HW, s.CH)...)
			}
		case m.Ambient != nil:
			tr.Ambient(m.Ambient.At.Add(shift), m.Ambient.TempC)
			r.ambients++
		}
	}
	return r
}

func ofs(d string) time.Time {
	x, err := time.ParseDuration(d)
	if err != nil {
		panic(err)
	}
	return t0.Add(x)
}

func (r run) between(id string, from, to time.Time) []snap {
	var out []snap
	for _, s := range r.snaps[id] {
		if !s.at.Before(from) && !s.at.After(to) {
			out = append(out, s)
		}
	}
	return out
}

func (r run) heatingStarts(id string) []Event {
	var out []Event
	for _, e := range ofType(r.events, EventHeatingStarted) {
		if e.DeviceID == id {
			out = append(out, e)
		}
	}
	return out
}

func peak(ss []snap) float64 {
	m := -1e9
	for _, s := range ss {
		if s.power != nil && *s.power > m {
			m = *s.power
		}
	}
	return m
}

func (r run) logHeating(t *testing.T) {
	t.Helper()
	for _, e := range r.events {
		if e.Type == EventStateChanged {
			p := "null"
			if e.PowerKW != nil {
				p = fmt.Sprintf("%.2f kW", *e.PowerKW)
			}
			t.Logf("%9v %-24s %-7s → %-7s source %-4q power %s", e.At.Sub(t0), e.DeviceID, e.Prev, e.State, e.Source, p)
		}
	}
}

// reheat-morning: CH_ON and HW_ON at 0:10:00, CH_OFF 0:55:02, HW_OFF
// 1:10:02. Both tanks heat during HW, with source hw.
func TestFixtureReheatMorning(t *testing.T) {
	tr := newTracker(t, planConfig())
	r := feed(t, tr, "reheat-morning", 0)
	r.logHeating(t)
	hwOn, hwOff := ofs("10m"), ofs("1h10m2s")
	for _, id := range []string{left, right} {
		hs := r.heatingStarts(id)
		if len(hs) != 1 || hs[0].Source != SourceHW || hs[0].At.Before(hwOn) || hs[0].At.After(hwOff) {
			t.Errorf("%s: heating_started %+v, want one, source hw, during HW", id, hs)
			continue
		}
		var heated, total int
		for _, s := range r.between(id, hs[0].At, hwOff) {
			total++
			if s.state == StateHeating {
				heated++
				if s.source != SourceHW {
					t.Errorf("%s %v: heating with source %q during HW", id, s.at.Sub(t0), s.source)
				}
			}
		}
		for _, s := range r.between(id, t0, hwOn) {
			if s.state == StateHeating {
				t.Errorf("%s %v: heating before HW_ON", id, s.at.Sub(t0))
			}
		}
		t.Logf("%s: heating from %v, %d of %d sweeps to HW_OFF heating (the rest the 3 L draw), peak %.2f kW",
			id, hs[0].At.Sub(t0), heated, total, peak(r.between(id, hwOn, r.end)))
		if heated < total*8/10 {
			t.Errorf("%s: only %d of %d sweeps heating from its start to HW_OFF", id, heated, total)
		}
	}
}

// ch-only-right-reheat (8 Nov, before the right valve was replaced): the
// CH-only spell 0:16:58–0:36:54 heats the right tank, source ch; the left
// tank doesn't heat; the 2-minute CH blip before it heats nothing.
func TestFixtureCHOnlyRightReheat(t *testing.T) {
	tr := newTracker(t, planConfig())
	r := feed(t, tr, "ch-only-right-reheat", 0)
	r.logHeating(t)
	if hs := r.heatingStarts(left); len(hs) != 0 {
		t.Errorf("left heating: %+v", hs)
	}
	t.Logf("left peak power %.2f kW", peak(r.snaps[left]))
	hs := r.heatingStarts(right)
	if len(hs) != 1 || hs[0].Source != SourceCH || hs[0].At.Before(ofs("16m58s")) || hs[0].At.After(ofs("36m54s")) {
		t.Fatalf("right heating_started %+v, want one, source ch, in the CH spell", hs)
	}
	t.Logf("right heating from %v, peak %.2f kW", hs[0].At.Sub(t0), peak(r.snaps[right]))
	for _, s := range r.snaps[right] {
		if s.state == StateHeating && s.source != SourceCH {
			t.Errorf("right %v: heating with source %q", s.at.Sub(t0), s.source)
		}
	}
}

// ch-only-no-heat (after the right valve was replaced): CH on for nearly
// two hours with HW off, and neither tank heats.
func TestFixtureCHOnlyNoHeat(t *testing.T) {
	tr := newTracker(t, planConfig())
	r := feed(t, tr, "ch-only-no-heat", 0)
	r.logHeating(t)
	for _, id := range []string{left, right} {
		if hs := r.heatingStarts(id); len(hs) != 0 {
			t.Errorf("%s heating: %+v", id, hs)
		}
		t.Logf("%s peak power %.2f kW", id, peak(r.snaps[id]))
	}
	if len(r.draws) == 0 {
		t.Error("no metered draws: the fixture's taps are missing")
	}
	// After the 4 L tap at 1:19 the right tank's bulk energy recovers (its
	// upper probe, on the return fitting, re-warms from 49 to 52 °C) at up
	// to 0.8 kW, while the power window still holds the draw. That is why
	// heating is judged only from a window clear of draws and their
	// catch-up, and why power is unknown there: no published power reaches
	// the heating threshold (TestNoHeatingJudgedFromADrawWindow pins the
	// rule synthetically).
	if p := peak(r.snaps[right]); p >= planConfig().HeatingOnKW {
		t.Errorf("right published peak %.2f kW: post-draw recovery published as power", p)
	}
}

// bath-in-reheat: the 72 L bath from 0:10:52 over 275.8 s, during an HW +
// CH reheat. Both tanks are drawing while it runs.
func TestFixtureBathInReheat(t *testing.T) {
	tr := newTracker(t, planConfig())
	r := feed(t, tr, "bath-in-reheat", 0)
	r.logHeating(t)
	var bath *meter.Draw
	for i := range r.draws {
		if r.draws[i].Litres == 72 {
			bath = &r.draws[i]
		}
	}
	if bath == nil {
		t.Fatalf("no 72 L draw in %+v", r.draws)
	}
	for _, id := range []string{left, right} {
		ss := r.between(id, bath.Start.Add(time.Second), bath.End)
		if len(ss) < 10 {
			t.Fatalf("%s: %d sweeps in the bath", id, len(ss))
		}
		for _, s := range ss {
			if s.state != StateDrawing {
				t.Errorf("%s %v: %s during the bath", id, s.at.Sub(t0), s.state)
			}
		}
	}
}

// night-loop-stopped then quiet-night, the next night: the right tank's
// loss steps from about the left's (its return loop stopped by air) to
// several times it (the loop running again). quiet-night is moved a day on,
// about the real gap between the two.
func TestFixtureUAFollowsTheLoopRestarting(t *testing.T) {
	tr := newTracker(t, planConfig())
	r1 := feed(t, tr, "night-loop-stopped", 0)
	l1, r1ua := mustStatus(t, tr, left).UA, mustStatus(t, tr, right).UA
	r2 := feed(t, tr, "quiet-night", 24*time.Hour)
	l2, r2ua := mustStatus(t, tr, left).UA, mustStatus(t, tr, right).UA
	t.Logf("night-loop-stopped: left %.2f W/K (%.1f h, prior %.2f), right %.2f W/K (%.1f h, prior %.2f)",
		l1.WPerK, l1.EvidenceHours, l1.PriorShare, r1ua.WPerK, r1ua.EvidenceHours, r1ua.PriorShare)
	t.Logf("+ quiet-night:      left %.2f W/K (%.1f h, prior %.2f), right %.2f W/K (%.1f h, prior %.2f)",
		l2.WPerK, l2.EvidenceHours, l2.PriorShare, r2ua.WPerK, r2ua.EvidenceHours, r2ua.PriorShare)
	for _, x := range []struct {
		name string
		r    run
	}{{"night-loop-stopped", r1}, {"quiet-night", r2}} {
		if x.r.ambients == 0 {
			t.Errorf("%s: no %s readings", x.name, ambientTopic)
		}
		if len(x.r.events) == 0 {
			t.Errorf("%s: no events at all", x.name)
		}
		for _, id := range []string{left, right} {
			if hs := x.r.heatingStarts(id); len(hs) != 0 {
				t.Errorf("%s: %s heating %+v", x.name, id, hs)
			}
		}
	}
	// Bands, from the data. The left tank is 4–5 W/K both nights (the
	// briefing's 3.2–4 W/K was against the colder hall). The right tank with
	// its loop stopped is 4–6 W/K hour by hour, like the left; the fit has
	// the prior (17 W/K, an hour's worth, 12% of the weight by morning) in
	// it too, so under 8.
	//
	// With the loop running, the right tank's hourly loss/ΔT against the
	// boiler-room sensor is 33–39 W/K, not the 15–19 W/K in PLAN.md: the
	// loss fits 18 W/K × (T − 14 °C), the slope PLAN's but the reference
	// some 10 °C below the room (28 → 23 °C overnight). The loop sheds its
	// heat into the house's pipework, not the boiler room, and the tank is
	// lukewarm (bulk mean 41 → 31 °C), so ΔT to the room is small. UA as
	// specified (against the room) is what the fit reports; after one night
	// it is most of the way there (the first night still has about a
	// quarter of the weight), so 20–40.
	if r1ua.WPerK >= 8 || r1ua.WPerK < 3 || r1ua.EvidenceHours < 5 {
		t.Errorf("night-loop-stopped: right %.2f W/K on %.1f h, want 3–8 on most of the night", r1ua.WPerK, r1ua.EvidenceHours)
	}
	if l1.WPerK < 3 || l1.WPerK > 6 {
		t.Errorf("night-loop-stopped: left %.2f W/K, want 3–6", l1.WPerK)
	}
	if r2ua.WPerK < 20 || r2ua.WPerK > 40 {
		t.Errorf("quiet-night: right %.2f W/K, want 20–40 (risen past PLAN.md's 15–19; see above)", r2ua.WPerK)
	}
	if l2.WPerK < 3 || l2.WPerK > 6 {
		t.Errorf("quiet-night: left %.2f W/K, want 3–6", l2.WPerK)
	}
}

// quiet-night alone: the fit against the room from one loop-running night.
func TestFixtureQuietNightAlone(t *testing.T) {
	tr := newTracker(t, planConfig())
	feed(t, tr, "quiet-night", 0)
	u := mustStatus(t, tr, right).UA
	t.Logf("quiet-night alone: right %.2f W/K (%.1f h, prior share %.2f)", u.WPerK, u.EvidenceHours, u.PriorShare)
	if u.WPerK < 28 || u.WPerK > 40 {
		t.Errorf("right %.2f W/K, want 28–40 (hourly 33–39, with the prior's share)", u.WPerK)
	}
}

// shower-layered: after the 50 L shower (last litre 0:17:46.6, draw_ended
// a minute later) the right tank's power is unknown through PowerSettle,
// then known again PowerMinSpan later, before the 15 L draw at 0:36:41.9.
// Before the settle's samples were dropped it stayed unknown for
// PowerSettle + PowerWindow, past the next draw: the whole 19 minutes.
func TestFixtureShowerPowerReturnsBeforeTheNextDraw(t *testing.T) {
	cfg := planConfig()
	tr := newTracker(t, cfg)
	r := feed(t, tr, "shower-layered", 0)
	var shower, next *meter.Draw
	for i := range r.draws {
		switch r.draws[i].Litres {
		case 50:
			shower = &r.draws[i]
		case 15:
			next = &r.draws[i]
		}
	}
	if shower == nil || next == nil {
		t.Fatalf("draws %+v, want the 50 L shower and the 15 L after it", r.draws)
	}
	ended := shower.End.Add(meterConfig().DrawEnd) // when the meter consumer ends it
	var first *snap
	for _, s := range r.between(right, ended, next.Start) {
		if s.power != nil {
			first = &s
			break
		}
	}
	if first == nil {
		t.Fatalf("right power unknown from the shower's end to the next draw at %v", next.Start.Sub(t0))
	}
	after := first.at.Sub(ended)
	t.Logf("right power known at %v, %v after draw_ended (%v after the last litre): %.2f kW, %s",
		first.at.Sub(t0), after, first.at.Sub(shower.End), *first.power, first.state)
	if after < cfg.PowerSettle+cfg.PowerMinSpan || after > cfg.PowerSettle+cfg.PowerMinSpan+time.Minute {
		t.Errorf("known %v after draw_ended, want PowerSettle + PowerMinSpan (%v) plus at most a minute of sweeps",
			after, cfg.PowerSettle+cfg.PowerMinSpan)
	}
	if first.state != StateCooling {
		t.Errorf("state %s when power returns, want cooling", first.state)
	}
}
