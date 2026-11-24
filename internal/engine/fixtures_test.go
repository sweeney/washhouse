package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/replay"
	"github.com/sweeney/washhouse/internal/thermal"
	"github.com/sweeney/washhouse/internal/vessel"
)

// PLAN.md's phase-2 assertions, over the recorded fixtures
// (testdata/fixtures, README.md there), replayed through the engine as the
// service will feed it.

const (
	fixtureDir   = "../../testdata/fixtures"
	ambientTopic = "zigbee2mqtt/climate_groundfloor_boiler-room"
)

var allFixtures = []string{"shower-layered", "bath-in-reheat", "bath-87", "taps-small", "creep-evening",
	"quiet-night", "reheat-morning", "ch-only-right-reheat", "ch-only-no-heat", "night-loop-stopped"}

// run is what a replay produced.
type run struct {
	events   []Event
	states   []State   // after every input
	inputEv  [][]Event // each input's events, by states' index
	ended    []DrawEnded
	balanced []DrawBalanced
	start    time.Time
	sweeps   map[string][]sweepSnap
}

// sweepSnap is a tank's raw probe readings and its profile at the
// readings' heights, after one of its sweeps.
type sweepSnap struct {
	at      time.Time
	raw     map[string]float64 // by position; missing if the read failed
	profile map[string]float64 // the strat profile at each position's height
	probes  []profile.Probe
	prof    *profile.Profile
	state   thermal.State
}

type feedOpts struct {
	shift     time.Duration // every time moved by this
	dropMeter bool          // leave the meter out
}

// feed replays fixture name into e, and finishes with a Tick far enough on
// for every draw to be attributed.
func feed(t *testing.T, e *Engine, name string, o feedOpts) run {
	t.Helper()
	msgs, err := replay.ReadFile(filepath.Join(fixtureDir, name+".log.gz"), replay.Options{Ambient: ambientTopic})
	if err != nil {
		t.Fatal(err)
	}
	r := run{start: t0.Add(o.shift), sweeps: map[string][]sweepSnap{}}
	var last time.Time
	collect := func(ev []Event) {
		r.events = append(r.events, ev...)
		for _, x := range ev {
			switch x.Type {
			case EventDrawEnded:
				r.ended = append(r.ended, *x.Ended)
			case EventDrawBalanced:
				r.balanced = append(r.balanced, *x.Balanced)
			}
		}
	}
	add := func(ev []Event) {
		collect(ev)
		s := e.State()
		r.states = append(r.states, s)
		r.inputEv = append(r.inputEv, ev)
		if s.At.After(last) {
			last = s.At
		}
	}
	for _, m := range msgs {
		shiftMsg(&m, o.shift)
		switch {
		case m.State != nil:
			ev, err := e.TankState(*m.State)
			if err != nil {
				t.Fatal(err)
			}
			add(ev)
			if sw, ok := e.LastSweep(m.State.DeviceID); ok && sw.At.Equal(m.State.Timestamp) {
				sn := sweepSnap{at: sw.At, raw: map[string]float64{}, profile: map[string]float64{}, prof: sw.Profile}
				for _, p := range sw.Probes {
					if p.TempC != nil {
						sn.raw[p.Position] = *p.TempC
					}
					sn.profile[p.Position] = sw.Profile.At(p.HeightMM)
					sn.probes = append(sn.probes, profile.Probe{HeightMM: p.HeightMM, TempC: p.TempC, Role: p.Role, Suspect: p.Suspect})
				}
				for _, tk := range e.State().Tanks {
					if tk.DeviceID == sw.DeviceID {
						sn.state = tk.State
					}
				}
				r.sweeps[sw.DeviceID] = append(r.sweeps[sw.DeviceID], sn)
			}
		case m.Meter != nil:
			if !o.dropMeter {
				add(e.Meter(*m.Meter))
			}
		case m.Status != nil:
			e.Status(*m.Status)
			add(nil)
		case m.BoilerEvent != nil:
			add(e.BoilerEvent(*m.BoilerEvent))
		case m.BoilerSystem != nil:
			add(e.BoilerSystem(*m.BoilerSystem))
		case m.Ambient != nil:
			add(e.Ambient(m.Ambient.At, m.Ambient.TempC))
		}
	}
	c := planConfig()
	collect(e.Tick(last.Add(c.Meter.DrawEnd + c.Draw.BalanceSettle + c.Draw.MaxSweepGap + time.Second)))
	return r
}

// shiftMsg moves every time in m by d.
func shiftMsg(m *replay.Msg, d time.Duration) {
	if d == 0 {
		return
	}
	mv := func(t *time.Time) {
		if t != nil {
			*t = t.Add(d)
		}
	}
	mv(&m.Received)
	switch {
	case m.State != nil:
		mv(&m.State.Timestamp)
	case m.Meter != nil:
		mv(&m.Meter.Timestamp)
		mv(&m.Meter.CountingSince)
		mv(m.Meter.LastPulseAt)
	case m.Status != nil:
		mv(m.Status.Timestamp)
	case m.BoilerEvent != nil:
		mv(&m.BoilerEvent.Timestamp)
	case m.BoilerSystem != nil:
		mv(&m.BoilerSystem.Timestamp)
		mv(m.BoilerSystem.StartTime)
	case m.Ambient != nil:
		mv(&m.Ambient.At)
	}
}

// replays caches replayOne: each fixture is replayed once per test binary
// (the runs are only read).
var replays = map[string]run{}

func replayOne(t *testing.T, name string) run {
	t.Helper()
	if r, ok := replays[name]; ok {
		return r
	}
	r := feed(t, newEngine(t, planConfig()), name, feedOpts{})
	replays[name] = r
	return r
}

func (r run) ofs(at time.Time) time.Duration { return at.Sub(r.start) }

// endedOf is the draw_ended of draw id.
func (r run) endedOf(t *testing.T, id string) DrawEnded {
	t.Helper()
	for _, d := range r.ended {
		if d.DrawID == id {
			return d
		}
	}
	t.Fatalf("no draw_ended for %s", id)
	return DrawEnded{}
}

// masked says whether b's shares are masked: a reheat, or HW unknown.
func masked(b DrawBalanced) bool {
	return b.MaskedByReheat || slices.Contains(b.Reasons, draw.ReasonHWUnknown)
}

func shareOf(d DrawBalanced, id string) *float64 {
	for _, x := range d.Tanks {
		if x.DeviceID == id {
			return x.Share
		}
	}
	return nil
}

// meterDraws replays a fixture's meter messages through a consumer alone:
// what the meter says the draws were.
func meterDraws(t *testing.T, name string) []meter.Draw {
	t.Helper()
	msgs, err := replay.ReadFile(filepath.Join(fixtureDir, name+".log.gz"), replay.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mc, err := meter.New(planConfig().Meter)
	if err != nil {
		t.Fatal(err)
	}
	var out []meter.Draw
	var last time.Time
	for _, m := range msgs {
		if m.Meter == nil {
			continue
		}
		last = m.Meter.Timestamp
		for _, e := range mc.Feed(*m.Meter) {
			if e.Type == meter.EventDrawEnded {
				out = append(out, *e.Draw)
			}
		}
	}
	for _, e := range mc.Tick(last.Add(time.Hour)) {
		if e.Type == meter.EventDrawEnded {
			out = append(out, *e.Draw)
		}
	}
	return out
}

// auditDraws is tools/meter_audit.py's reading of the fixtures
// (draws.json): its multi-litre draws, which the meter consumer matches
// draw for draw.
func auditDraws(t *testing.T) map[string][]int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, "draws.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]struct {
		Draws []struct {
			Litres int `json:"litres"`
		} `json:"draws"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	out := map[string][]int{}
	for name, f := range m {
		for _, d := range f.Draws {
			if d.Litres >= 2 {
				out[name] = append(out[name], d.Litres)
			}
		}
	}
	return out
}

// 1. Every draw's litres are the meter's, and its kind is right.
func TestFixtureDrawsAreTheMeters(t *testing.T) {
	audit := auditDraws(t)
	for _, name := range allFixtures {
		t.Run(name, func(t *testing.T) {
			r := replayOne(t, name)
			want := meterDraws(t, name)
			if len(r.ended) != len(want) {
				t.Fatalf("%d draws ended, the meter has %d", len(r.ended), len(want))
			}
			var multi []int
			for i, d := range r.ended {
				w := want[i]
				if d.DrawID != w.ID || d.Litres != w.Litres || !d.StartedAt.Equal(w.Start) || !d.EndedAt.Equal(w.End) ||
					d.Kind != w.Kind || d.VolumeKind != "metered" || d.From != w.From || d.To != w.To {
					t.Errorf("draw %d: %s %v L %s %v–%v, meter %s %v L %s %v–%v", i, d.DrawID, d.Litres, d.Kind,
						r.ofs(d.StartedAt), r.ofs(d.EndedAt), w.ID, w.Litres, w.Kind, r.ofs(w.Start), r.ofs(w.End))
				}
				if d.Litres >= 2 {
					multi = append(multi, int(d.Litres))
				}
				t.Logf("+%-9v %3.0f L %-6s", r.ofs(d.StartedAt).Truncate(time.Second), d.Litres, d.Kind)
			}
			if !slices.Equal(multi, audit[name]) {
				t.Errorf("multi-litre draws %v, meter_audit.py %v", multi, audit[name])
			}
			kinds := map[meter.Kind][]float64{}
			for _, d := range r.ended {
				kinds[d.Kind] = append(kinds[d.Kind], d.Litres)
			}
			switch name {
			case "shower-layered":
				if !slices.Equal(kinds[meter.KindShower], []float64{50}) || len(kinds[meter.KindBath]) != 0 {
					t.Errorf("kinds %v, want one 50 L shower and the rest taps", kinds)
				}
			case "bath-in-reheat", "bath-87":
				l := map[string]float64{"bath-in-reheat": 72, "bath-87": 87}[name]
				if !slices.Equal(kinds[meter.KindBath], []float64{l}) || len(kinds[meter.KindShower]) != 0 {
					t.Errorf("kinds %v, want one %v L bath and the rest taps", kinds, l)
				}
			case "taps-small":
				if len(r.ended) == 0 || len(kinds[meter.KindTap]) != len(r.ended) {
					t.Errorf("kinds %v, want taps only", kinds)
				}
			}
		})
	}
}

// 2. The right tank's share, from the balance sweep, is about 0.89
// (0.85–0.95) on every unmasked draw of 10 L or more whose before sweep
// had no cold layer in a gap (internal/draw's TestFixtureShareDistribution
// says why that one is left out). Masked draws have no shares.
func TestFixtureRightShare(t *testing.T) {
	n := 0
	for _, name := range allFixtures {
		r := replayOne(t, name)
		for _, b := range r.balanced {
			d := r.endedOf(t, b.DrawID)
			s := shareOf(b, right)
			if masked(b) {
				if s != nil {
					t.Errorf("%s %v L: masked %v, yet right share %.3f", name, d.Litres, b.Reasons, *s)
				}
				continue
			}
			if d.Litres < 10 {
				continue
			}
			t.Logf("%s %v L: right share %s %v", name, d.Litres, fm(s, "%.3f"), b.Reasons)
			if slices.Contains(b.Reasons, draw.ReasonThermocline) {
				continue
			}
			n++
			if s == nil || *s < 0.85 || *s > 0.95 {
				t.Errorf("%s %v L: right share %s, want 0.85–0.95", name, d.Litres, fm(s, "%.3f"))
			}
		}
	}
	if n < 2 {
		t.Errorf("only %d unmasked draws of 10 L or more", n)
	}
}

// draw_balanced follows each draw_ended with the same draw_id and an
// identical ended_at, and comes no earlier.
func TestFixtureBalancedFollowsEnded(t *testing.T) {
	for _, name := range allFixtures {
		r := replayOne(t, name)
		if len(r.balanced) != len(r.ended) {
			t.Errorf("%s: %d draw_balanced for %d draw_ended", name, len(r.balanced), len(r.ended))
			continue
		}
		at := map[string]time.Time{}
		for _, x := range r.events {
			switch x.Type {
			case EventDrawEnded:
				at[x.Ended.DrawID] = x.At
			case EventDrawBalanced:
				ea, ok := at[x.Balanced.DrawID]
				if !ok || x.At.Before(ea) {
					t.Errorf("%s: draw_balanced %s at %v, draw_ended at %v (%v)", name, x.Balanced.DrawID, x.At, ea, ok)
				}
				if d := r.endedOf(t, x.Balanced.DrawID); !d.EndedAt.Equal(x.Balanced.EndedAt) {
					t.Errorf("%s: %s ended_at %v in draw_balanced, %v in draw_ended", name, d.DrawID, x.Balanced.EndedAt, d.EndedAt)
				}
				t.Logf("%s %s: draw_ended +%v after the last litre, draw_balanced +%v", name, x.Balanced.DrawID,
					ea.Sub(x.Balanced.EndedAt).Truncate(time.Second), x.At.Sub(x.Balanced.EndedAt).Truncate(time.Second))
			}
		}
	}
}

// 3. The heat balance is within 10% and reliable on the 50 L shower, and
// unreliable, with reasons, on the reheat baths, every small draw and the
// 15 L draw after the shower.
func TestFixtureHeatBalance(t *testing.T) {
	sh := replayOne(t, "shower-layered")
	for _, d := range sh.balanced {
		switch sh.endedOf(t, d.DrawID).Litres {
		case 50:
			t.Logf("shower: heat balance %s L, reliable %v %v", fm(d.HeatBalanceL, "%.1f"), d.HeatBalanceReliable, d.Reasons)
			if d.HeatBalanceL == nil || math.Abs(*d.HeatBalanceL/50-1) > 0.10 || !d.HeatBalanceReliable || len(d.Reasons) != 0 {
				t.Errorf("shower: heat balance %s L, reliable %v %v, want within 10%% and reliable", fm(d.HeatBalanceL, "%.1f"), d.HeatBalanceReliable, d.Reasons)
			}
		case 15:
			t.Logf("15 L after the shower: heat balance %s L, reliable %v %v", fm(d.HeatBalanceL, "%.1f"), d.HeatBalanceReliable, d.Reasons)
			if d.HeatBalanceReliable || !slices.Contains(d.Reasons, draw.ReasonThermocline) {
				t.Errorf("15 L after the shower: reliable %v %v, want unreliable for the thermocline", d.HeatBalanceReliable, d.Reasons)
			}
		}
	}
	for name, l := range map[string]float64{"bath-in-reheat": 72, "bath-87": 87} {
		found := false
		r := replayOne(t, name)
		for _, d := range r.balanced {
			if r.endedOf(t, d.DrawID).Litres != l {
				continue
			}
			found = true
			t.Logf("%s: heat balance %s L, reliable %v %v, masked %v", name, fm(d.HeatBalanceL, "%.1f"), d.HeatBalanceReliable, d.Reasons, d.MaskedByReheat)
			if d.HeatBalanceReliable || !d.MaskedByReheat || !slices.Contains(d.Reasons, draw.ReasonHWOn) {
				t.Errorf("%s: reliable %v masked %v %v, want masked by HW", name, d.HeatBalanceReliable, d.MaskedByReheat, d.Reasons)
			}
		}
		if !found {
			t.Errorf("%s: no %v L draw", name, l)
		}
	}
	small := 0
	for _, name := range allFixtures {
		r := replayOne(t, name)
		for _, d := range r.balanced {
			e := r.endedOf(t, d.DrawID)
			if e.Litres >= planConfig().Draw.MinReliableLitres {
				continue
			}
			small++
			if d.HeatBalanceReliable || !slices.Contains(d.Reasons, draw.ReasonSmall) {
				t.Errorf("%s %v L: reliable %v %v, want small", name, e.Litres, d.HeatBalanceReliable, d.Reasons)
			}
		}
	}
	if small < 10 {
		t.Errorf("only %d small draws", small)
	}
}

// 4. creep-evening: the lone litres raise continuous_flow, and no draw is
// made of them.
func TestFixtureCreep(t *testing.T) {
	r := replayOne(t, "creep-evening")
	started := ofType(r.events, EventContinuousFlowStarted)
	ended := ofType(r.events, EventContinuousFlowEnded)
	if len(started) == 0 || len(ended) != len(started) {
		t.Fatalf("continuous_flow_started %d, _ended %d: want at least one of each, paired", len(started), len(ended))
	}
	for i := range started {
		t.Logf("continuous flow +%v to +%v, %v L at %.2f L/min", r.ofs(started[i].Flow.Since).Truncate(time.Second),
			r.ofs(ended[i].At).Truncate(time.Second), ended[i].Flow.Litres, ended[i].Flow.FlowLPerMin)
	}
	creep := []time.Duration{10*time.Minute + 25848*time.Millisecond, 45*time.Minute + 18378*time.Millisecond,
		time.Hour + 48378*time.Millisecond}
	if got := r.ofs(started[0].Flow.Since); got != creep[0] {
		t.Errorf("the first continuous flow from +%v, want the first lone litre +%v", got, creep[0])
	}
	for _, d := range r.ended {
		for _, c := range creep {
			if at := r.start.Add(c); !at.Before(d.StartedAt) && !at.After(d.EndedAt) {
				t.Errorf("draw %s (+%v, %v L) holds the creep litre at +%v", d.DrawID, r.ofs(d.StartedAt), d.Litres, c)
			}
		}
	}
	for _, x := range ofType(r.events, EventDrawStarted) {
		for _, c := range creep {
			if x.Draw.StartedAt.Equal(r.start.Add(c)) {
				t.Errorf("draw_started at the creep litre +%v", c)
			}
		}
	}
	seen := false
	for _, s := range r.states {
		if s.Meter.ContinuousFlow != nil {
			seen = true
			if s.ActiveDraw != nil && !s.ActiveDraw.StartedAt.After(s.Meter.ContinuousFlow.Since) {
				t.Errorf("active draw %+v from the creep", s.ActiveDraw)
			}
		}
	}
	if !seen {
		t.Error("the state never shows the continuous flow")
	}
}

// 5. No draws on the quiet nights.
func TestFixtureQuietNightsHaveNoDraws(t *testing.T) {
	for _, name := range []string{"quiet-night", "night-loop-stopped"} {
		r := replayOne(t, name)
		for _, ty := range []EventType{EventDrawStarted, EventDrawUpdated, EventDrawEnded, EventDrawBalanced, EventContinuousFlowStarted} {
			if n := len(ofType(r.events, ty)); n != 0 {
				t.Errorf("%s: %d %s", name, n, ty)
			}
		}
		for _, s := range r.states {
			if s.ActiveDraw != nil {
				t.Errorf("%s: active draw %+v", name, s.ActiveDraw)
				break
			}
		}
	}
}

// 6. The right tank's upper probe (P4) sits on the secondary return and
// reads low. The fair check of "corrected by strat": against the line P3
// and P5 draw through P4's height (what the profile would be without P4),
// raw P4 dips 5–9 °C below it; with strat the profile at P4 is never below
// P3 (no dip), and stays within StratResidualC of that line. It does not
// reach the line: at rest P5, though heat-sunk, reads above P3, and strat
// lifts P4 only to P3. The stored energy with P4 under strat is within
// 0.1 kWh (about 2 L of 40 °C water) of the energy without P4. Sweeps
// while drawing are left out (P5 reads the true top then).
func TestFixtureRightP4GapCorrectedByStrat(t *testing.T) {
	const stratResidualC, energyKWh = 2.5, 0.1
	cfg := planConfig()
	v, err := vessel.New(cfg.Vessel)
	if err != nil {
		t.Fatal(err)
	}
	est, err := estimate.New(v, cfg.Estimate)
	if err != nil {
		t.Fatal(err)
	}
	var rawDip, stratDip, dE []float64
	for _, name := range allFixtures {
		r := replayOne(t, name)
		for _, s := range r.sweeps[right] {
			p3, ok3 := s.raw["middle"]
			p4, ok4 := s.raw["upper"]
			p5, ok5 := s.raw["top"]
			if !ok3 || !ok4 || !ok5 || !(p4 < p3) {
				continue
			}
			if got := s.profile["upper"]; got < p3 {
				t.Errorf("%s +%v: profile at P4 %.2f below P3 %.2f", name, r.ofs(s.at), got, p3)
			}
			if s.state == thermal.StateDrawing {
				continue
			}
			line := p3 + (p5-p3)*(945-825)/(1215-825)
			rawDip = append(rawDip, line-p4)
			stratDip = append(stratDip, line-s.profile["upper"])
			if d := line - s.profile["upper"]; d > stratResidualC {
				t.Errorf("%s +%v: profile at P4 %.2f °C below the P3–P5 line", name, r.ofs(s.at), d)
			}
			noP4, err := profile.New(s.probes, profile.Options{Exclude: []profile.Role{profile.RoleReturn}})
			if err != nil {
				t.Fatal(err)
			}
			withP4, err := profile.New(s.probes, profile.Options{})
			if err != nil {
				t.Fatal(err)
			}
			d := est.Assess(withP4).KWh - est.Assess(noP4).KWh
			dE = append(dE, d)
			if math.Abs(d) > energyKWh {
				t.Errorf("%s +%v: energy with P4 %.3f kWh off the energy without it", name, r.ofs(s.at), d)
			}
		}
	}
	if len(rawDip) < 1000 {
		t.Fatalf("only %d resting sweeps with the P4 gap", len(rawDip))
	}
	q := func(xs []float64) string {
		xs = slices.Clone(xs)
		slices.Sort(xs)
		n := len(xs)
		return fmt.Sprintf("median %.2f, p90 %.2f, max %.2f", xs[n/2], xs[n*9/10], xs[n-1])
	}
	t.Logf("%d resting right sweeps with P4 below P3. Below the P3–P5 line: raw P4 %s °C; strat %s °C. Energy with P4 less without: %s kWh",
		len(rawDip), q(rawDip), q(stratDip), q(dE))
	if med := func(xs []float64) float64 { xs = slices.Clone(xs); slices.Sort(xs); return xs[len(xs)/2] }; med(stratDip) > med(rawDip)/2 {
		t.Errorf("strat closes less than half the median dip")
	}
}

// 7. CH-only heating is attributed to the right tank when it happens, and
// none is claimed when it doesn't.
func TestFixtureCHOnly(t *testing.T) {
	heating := func(r run, id string) []Event {
		var out []Event
		for _, x := range ofType(r.events, EventHeatingStarted) {
			if x.Tank.DeviceID == id {
				out = append(out, x)
			}
		}
		return out
	}
	r := replayOne(t, "ch-only-right-reheat")
	if hs := heating(r, left); len(hs) != 0 {
		t.Errorf("ch-only-right-reheat: left heating %+v", hs)
	}
	hs := heating(r, right)
	if len(hs) != 1 || hs[0].Tank.Source == nil || *hs[0].Tank.Source != thermal.SourceCH {
		t.Fatalf("ch-only-right-reheat: right heating_started %+v, want one, source ch", hs)
	}
	t.Logf("ch-only-right-reheat: right heating from +%v", r.ofs(hs[0].At))
	heated := 0
	for _, s := range r.states {
		for _, tk := range s.Tanks {
			if tk.State != thermal.StateHeating {
				continue
			}
			if tk.DeviceID == left {
				t.Errorf("left heating in the state at +%v", r.ofs(s.At))
			}
			if tk.HeatSource == nil || *tk.HeatSource != thermal.SourceCH {
				t.Errorf("%s heating at +%v with source %v", tk.DeviceID, r.ofs(s.At), tk.HeatSource)
			}
			heated++
		}
	}
	if heated == 0 {
		t.Error("the state never shows the right tank heating")
	}
	r = replayOne(t, "ch-only-no-heat")
	for _, id := range []string{left, right} {
		if hs := heating(r, id); len(hs) != 0 {
			t.Errorf("ch-only-no-heat: %s heating %+v", id, hs)
		}
	}
	for _, s := range r.states {
		for _, tk := range s.Tanks {
			if tk.State == thermal.StateHeating || tk.HeatSource != nil {
				t.Errorf("ch-only-no-heat: %s %s source %v at +%v", tk.DeviceID, tk.State, tk.HeatSource, r.ofs(s.At))
			}
		}
	}
}

// 8. The loss fit follows the right tank's loop restarting:
// night-loop-stopped, then quiet-night a day later (about the real gap),
// through one engine. quiet-night's meter is left out: its run began
// before the fixture with 239 litres counted, and after night-loop-stopped's
// run that reads as a restart lived through, all of it news (neither night
// has a draw; test 5).
func TestFixtureUAFollowsTheLoop(t *testing.T) {
	e := newEngine(t, planConfig())
	ua := func() (l, r float64) {
		s := e.State()
		return *s.Tanks[0].UAWPerK, *s.Tanks[1].UAWPerK
	}
	feed(t, e, "night-loop-stopped", feedOpts{})
	l1, r1 := ua()
	feed(t, e, "quiet-night", feedOpts{shift: 24 * time.Hour, dropMeter: true})
	l2, r2 := ua()
	t.Logf("night-loop-stopped: left %.2f W/K, right %.2f W/K; then quiet-night: left %.2f W/K, right %.2f W/K", l1, r1, l2, r2)
	// internal/thermal's bands (TestFixtureUAFollowsTheLoopRestarting,
	// which says why the loop-running value is against the room).
	if r1 < 3 || r1 >= 8 {
		t.Errorf("night-loop-stopped: right %.2f W/K, want 3–8", r1)
	}
	if r2 < 20 || r2 > 40 {
		t.Errorf("then quiet-night: right %.2f W/K, want 20–40", r2)
	}
	for _, x := range []float64{l1, l2} {
		if x < 3 || x > 6 {
			t.Errorf("left %.2f W/K, want 3–6", x)
		}
	}
}

// 9. low_hot_water is the system's: its usable water is the tanks' summed.
// The tanks are in parallel on one outlet. The owner's experience (24 Nov
// 2026): with one tank cold and the other hot, the taps still run hot, so
// the flow isn't held at a fixed split; the hot tank supplies the tap. So
// the system's usable_l40 is the tanks' usable_l40 summed (each tank's
// drawable water mixed down to MixC), not a draw at a fixed split. On
// quiet-night the right tank cools overnight with its loop running while
// the left stays hot: by the morning the left alone holds plenty, so the
// system isn't low (the fixed-split model had said it was).
func TestFixtureLowHotWaterIsTheSystems(t *testing.T) {
	cfg := planConfig()
	r := replayOne(t, "quiet-night")
	var prev *bool
	var first, last *State
	for i := range r.states {
		s := &r.states[i]
		if s.UsableL40 != nil {
			var sum, sumLow float64
			for _, tk := range s.Tanks {
				sum += *tk.UsableL40
				sumLow += *tk.UsableL40Low
			}
			if math.Abs(*s.UsableL40-sum) > 1e-9 || math.Abs(*s.UsableL40Low-sumLow) > 1e-9 {
				t.Fatalf("+%v: system usable_l40 %.3f / low %.3f, the tanks sum to %.3f / %.3f",
					r.ofs(s.At), *s.UsableL40, *s.UsableL40Low, sum, sumLow)
			}
		}
		if s.LowHotWater == nil {
			continue
		}
		if first == nil {
			first = s
		}
		last = s
		if prev == nil || *prev != *s.LowHotWater {
			t.Logf("+%v low_hot_water %v: system usable_l40_low %.1f L (left %.1f, right %.1f)",
				r.ofs(s.At).Truncate(time.Second), *s.LowHotWater, *s.UsableL40Low, *s.Tanks[0].UsableL40Low, *s.Tanks[1].UsableL40Low)
		}
		prev = s.LowHotWater
	}
	if first == nil {
		t.Fatal("low_hot_water never known")
	}
	lt, rt := last.Tanks[0], last.Tanks[1]
	t.Logf("at the end (+%v): left top %.1f °C usable_l40 %.1f L, right top %.1f °C usable_l40 %.1f L, system %.1f L",
		r.ofs(last.At).Truncate(time.Second), *lt.TopC, *lt.UsableL40, *rt.TopC, *rt.UsableL40, *last.UsableL40)
	if *first.LowHotWater {
		t.Errorf("at the start: low, with %.1f L to plan on", *first.UsableL40Low)
	}
	if *last.LowHotWater || !(*lt.UsableL40Low > cfg.LowHotWaterL+cfg.LowHotWaterMarginL) {
		t.Errorf("at the end: low %v, left alone %.1f L: the left tank's hot water should keep the system off low", *last.LowHotWater, *lt.UsableL40Low)
	}
}

// 10. What statehouse needs from the retained state: active_draw carries
// draw_started's draw_id and started_at, and every state has a real
// timestamp (event time, never the wall clock or zero).
func TestFixtureRetainedStateFacts(t *testing.T) {
	for _, name := range allFixtures {
		r := replayOne(t, name)
		starts := map[string]time.Time{}
		for _, x := range ofType(r.events, EventDrawStarted) {
			starts[x.Draw.DrawID] = x.Draw.StartedAt
			if !x.At.Equal(x.Draw.StartedAt) {
				t.Errorf("%s: draw_started at %v, its draw from %v", name, x.At, x.Draw.StartedAt)
			}
		}
		seen := map[string]bool{}
		lo, hi := r.start.Add(-48*time.Hour), r.start.Add(12*time.Hour)
		for _, s := range r.states {
			if s.At.Before(lo) || s.At.After(hi) {
				t.Fatalf("%s: state at %v, outside the fixture's time", name, s.At)
			}
			if a := s.ActiveDraw; a != nil {
				st, ok := starts[a.DrawID]
				if !ok || !st.Equal(a.StartedAt) {
					t.Errorf("%s: active draw %s from %v, draw_started %v (%v)", name, a.DrawID, a.StartedAt, st, ok)
				}
				seen[a.DrawID] = true
			}
			for _, tk := range s.Tanks {
				if tk.At != nil && tk.At.After(s.At) {
					t.Errorf("%s: tank %s at %v after the state's %v", name, tk.DeviceID, *tk.At, s.At)
				}
			}
		}
		for id := range starts {
			if !seen[id] {
				t.Errorf("%s: draw %s never in active_draw", name, id)
			}
		}
		// A draw stays in active_draw from its draw_started until the input
		// that gives its draw_ended, and is gone from that input on.
		firstIn, endIn := map[string]int{}, map[string]int{}
		for i, s := range r.states {
			if a := s.ActiveDraw; a != nil {
				if _, ok := firstIn[a.DrawID]; !ok {
					firstIn[a.DrawID] = i
				}
				if _, ended := endIn[a.DrawID]; ended {
					t.Errorf("%s: draw %s in active_draw at +%v, after its draw_ended", name, a.DrawID, r.ofs(s.At))
				}
			}
			for _, x := range r.inputEv[i] {
				if x.Type == EventDrawEnded {
					endIn[x.Ended.DrawID] = i
				}
			}
		}
		for id, from := range firstIn {
			to, ok := endIn[id]
			if !ok {
				to = len(r.states)
			}
			for i := from; i < to; i++ {
				if a := r.states[i].ActiveDraw; a == nil || a.DrawID != id {
					t.Errorf("%s: draw %s left active_draw at +%v, before its draw_ended", name, id, r.ofs(r.states[i].At))
					break
				}
			}
			if ok && to > 0 {
				if a := r.states[to].ActiveDraw; a != nil && a.DrawID == id {
					t.Errorf("%s: draw %s still active in the state of its draw_ended", name, id)
				}
			}
		}
		for _, d := range r.ended {
			if _, ok := starts[d.DrawID]; !ok {
				t.Errorf("%s: draw_ended %s without a draw_started", name, d.DrawID)
			}
		}
		if len(r.states) == 0 || strings.Contains(name, "ch-only-right") && len(starts) != 0 {
			t.Errorf("%s: %d states, %d draws", name, len(r.states), len(starts))
		}
	}
}
