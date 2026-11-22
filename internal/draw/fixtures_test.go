package draw

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/replay"
	"github.com/sweeney/washhouse/internal/tank"
	"github.com/sweeney/washhouse/internal/vessel"
)

// The recorded fixtures (testdata/fixtures, real data shifted to a fake
// epoch) replayed through tank, meter and the Attributor, as the engine
// will: PLAN.md's phase-2 assertions for attribution.

const fixtureDir = "../../testdata/fixtures"

// planMeterConfig is internal/meter's planConfig: PLAN.md's thresholds.
func planMeterConfig() meter.Config {
	return meter.Config{
		DeviceID:             "hot_water_meter",
		DrawEnd:              60 * time.Second,
		BathFlowLPerMin:      10,
		BathMinLitres:        20,
		ShowerMinLitres:      20,
		ShowerMinDuration:    180 * time.Second,
		ShowerMinFlowLPerMin: 3,
		CreepClosed:          10 * time.Second,
		CreepWindow:          2700 * time.Second,
		StaleAfter:           90 * time.Second,
	}
}

// houseTanks is internal/tank's houseTanks: five probes each at the same
// heights; the right tank's upper probe is on the secondary return. The
// vessel is avail.py's STI 210, drawable from the cold inlet to the outlet.
func houseTanks(t *testing.T) map[string]*tank.Tank {
	t.Helper()
	v, err := vessel.New(vessel.Params{CapacityL: 200, InnerDiameterMM: 460, DomeDepthMM: 60, Z0MM: 150, SliceMM: 1})
	if err != nil {
		t.Fatal(err)
	}
	est, err := estimate.New(v, estimate.Config{ColdC: 10, MinC: 40, MixC: 40, KWhPerLitreK: 4.186 / 3600,
		LowFactor: 0.85, DrawFromMM: 281, DrawToMM: 1237})
	if err != nil {
		t.Fatal(err)
	}
	probes := func(upper profile.Role) []tank.ProbeConfig {
		return []tank.ProbeConfig{
			{Position: "bottom", Role: profile.RoleInlet, HeightMM: 265},
			{Position: "lower", Role: profile.RoleBulk, HeightMM: 400},
			{Position: "middle", Role: profile.RoleBulk, HeightMM: 825},
			{Position: "upper", Role: upper, HeightMM: 945},
			{Position: "top", Role: profile.RoleOutlet, HeightMM: 1215},
		}
	}
	out := map[string]*tank.Tank{}
	for id, role := range map[string]profile.Role{left: profile.RoleBulk, right: profile.RoleReturn} {
		tk, err := tank.New(tank.Config{DeviceID: id, Probes: probes(role)}, est)
		if err != nil {
			t.Fatal(err)
		}
		out[id] = tk
	}
	return out
}

// replayFixture feeds a fixture through tank, meter and an Attributor with
// cfg, and returns every attribution and every ended draw.
func replayFixture(t *testing.T, name string, cfg Config) ([]Attribution, []meter.Draw) {
	t.Helper()
	msgs, err := replay.ReadFile(filepath.Join(fixtureDir, name+".log.gz"), replay.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tanks := houseTanks(t)
	mc, err := meter.New(planMeterConfig())
	if err != nil {
		t.Fatal(err)
	}
	at, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var rs []Result
	var draws []meter.Draw
	var last time.Time
	for _, m := range msgs {
		switch {
		case m.State != nil:
			s, err := tanks[m.State.DeviceID].Sweep(*m.State)
			if err != nil {
				t.Fatal(err)
			}
			rs = append(rs, at.Sweep(s)...)
			last = s.At
		case m.Meter != nil:
			for _, e := range mc.Feed(*m.Meter) {
				if e.Type == meter.EventDrawEnded {
					draws = append(draws, *e.Draw)
				}
				rs = append(rs, at.Meter(e)...)
			}
		case m.BoilerEvent != nil:
			at.HW(m.BoilerEvent.Timestamp, m.BoilerEvent.HW)
		case m.BoilerSystem != nil && m.BoilerSystem.ReadyHeartbeat():
			// the engine's rule (boiler.System.ReadyHeartbeat)
			at.HW(m.BoilerSystem.Timestamp, m.BoilerSystem.HW)
		}
	}
	for _, e := range mc.Tick(last.Add(time.Hour)) {
		if e.Type == meter.EventDrawEnded {
			draws = append(draws, *e.Draw)
		}
		rs = append(rs, at.Meter(e)...)
	}
	rs = append(rs, at.Tick(last.Add(time.Hour))...)
	// Every draw is ended, then balanced, once each.
	seen := map[string]Stage{}
	var out []Attribution
	for _, r := range rs {
		id := r.Draw.ID
		if r.Stage == StageBalanced {
			id = r.Attribution.Draw.ID
			out = append(out, r.Attribution)
		}
		if r.Stage != seen[id]+1 {
			t.Fatalf("%s: draw %s stage %d after %d", name, id, r.Stage, seen[id])
		}
		seen[id] = r.Stage
	}
	if len(out) != len(draws) {
		t.Fatalf("%s: %d attributions for %d draws", name, len(out), len(draws))
	}
	return out, draws
}

func describe(t0 time.Time, a Attribution) string {
	var b strings.Builder
	fmt.Fprintf(&b, "+%-9s %3.0f L %-6s", a.Draw.Start.Sub(t0).Truncate(time.Second), a.Draw.Litres, a.Draw.Kind)
	if a.HeatBalanceL != nil {
		fmt.Fprintf(&b, " hb %5.1f L (%+4.0f%%)", *a.HeatBalanceL, 100*(*a.HeatBalanceL/a.Draw.Litres-1))
	} else {
		fmt.Fprintf(&b, " hb     -        ")
	}
	for _, x := range a.Tanks {
		side := x.DeviceID[strings.LastIndex(x.DeviceID, "_")+1:]
		fmt.Fprintf(&b, " | %s share %s lost %s/%s peak %s drop %s", side[:1], fm(x.Share, "%.2f"), fm(x.EarlyLostKWh, "%.2f"), fm(x.SettledLostKWh, "%.2f"), fm(x.OutletPeakC, "%.1f"), fm(x.InletDropC, "%.1f"))
	}
	fmt.Fprintf(&b, " | out %s E %s reliable %v %v", fm(a.OutletC, "%.1f"), fm(a.EnergyKWh, "%.2f"), a.Reliable, a.Reasons)
	return b.String()
}

func fm(p *float64, format string) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf(format, *p)
}

var fixtures = []string{"shower-layered", "bath-in-reheat", "bath-87", "taps-small", "creep-evening",
	"quiet-night", "reheat-morning", "ch-only-no-heat", "night-loop-stopped"}

// Every fixture: one attribution per metered draw, carrying the meter's
// litres, logged for the report.
func TestFixturesAttributeEveryDraw(t *testing.T) {
	for _, name := range fixtures {
		got, draws := replayFixture(t, name, testConfig())
		var t0 time.Time
		if len(draws) > 0 {
			t0 = draws[0].Start.Truncate(24 * time.Hour)
		}
		for i, a := range got {
			if a.Draw.ID != draws[i].ID || a.Draw.Litres != draws[i].Litres {
				t.Errorf("%s: attribution %d is draw %s %v L, want %s %v L", name, i, a.Draw.ID, a.Draw.Litres, draws[i].ID, draws[i].Litres)
			}
			t.Logf("%-15s %s", name, describe(t0, a))
			// Only a reheat makes a tank gain over a draw: the heat-sunk
			// outlet reading true during a tap must not (bulk energy).
			if slices.Contains(a.Reasons, ReasonGained) && !slices.Contains(a.Reasons, ReasonHWOn) {
				t.Errorf("%s: %s gained heat with HW off: %v", name, a.Draw.ID, a.Reasons)
			}
		}
	}
}

func drawOf(t *testing.T, as []Attribution, litres float64) Attribution {
	t.Helper()
	for _, a := range as {
		if a.Draw.Litres == litres {
			return a
		}
	}
	t.Fatalf("no %v L draw", litres)
	return Attribution{}
}

// The 50 L shower from a layered tank: the heat balance is within 10% and
// reliable, and the right tank supplied most of it: 0.89 from the balance
// sweep (tanktemp's review of PR #2 found the same, from the bulk energy at
// +60 s; the +60 s full estimate read 0.76 on a picture 26% short). The 15 L draw after it
// is unreliable: its cold layer sat in the gap between P2 and P3.
func TestFixtureShowerLayered(t *testing.T) {
	as, _ := replayFixture(t, "shower-layered", testConfig())
	sh := drawOf(t, as, 50)
	if sh.HeatBalanceL == nil || *sh.HeatBalanceL < 45 || *sh.HeatBalanceL > 55 || !sh.Reliable {
		t.Errorf("shower: heat balance %s L reliable %v %v, want 45–55 L and reliable", fm(sh.HeatBalanceL, "%.1f"), sh.Reliable, sh.Reasons)
	}
	if r := tankOf(t, sh, right); r.Share == nil || *r.Share < 0.85 || *r.Share > 0.93 {
		t.Errorf("shower: right share %s, want 0.85–0.93", fm(r.Share, "%.3f"))
	}
	after := drawOf(t, as, 15)
	if after.Reliable || !slices.Contains(after.Reasons, ReasonThermocline) || !slices.Contains(after.Reasons, ReasonSmall) {
		t.Errorf("15 L after the shower: reliable %v %v, want unreliable for the thermocline and its size", after.Reliable, after.Reasons)
	}
}

// The 72 L bath during a reheat: masked, unreliable. The 87 L bath (still
// in that reheat): masked too.
func TestFixtureBathsInReheat(t *testing.T) {
	for _, tc := range []struct {
		name   string
		litres float64
	}{{"bath-in-reheat", 72}, {"bath-87", 87}} {
		as, _ := replayFixture(t, tc.name, testConfig())
		b := drawOf(t, as, tc.litres)
		if !b.MaskedByReheat || b.Reliable || !slices.Contains(b.Reasons, ReasonHWOn) {
			t.Errorf("%s: masked %v reliable %v %v, want masked by HW and unreliable", tc.name, b.MaskedByReheat, b.Reliable, b.Reasons)
		}
	}
}

// Every draw in taps-small is under 20 L, so none has a reliable balance.
func TestFixtureTapsSmall(t *testing.T) {
	as, _ := replayFixture(t, "taps-small", testConfig())
	if len(as) == 0 {
		t.Fatal("no draws")
	}
	for _, a := range as {
		if a.Reliable || !slices.Contains(a.Reasons, ReasonSmall) {
			t.Errorf("%v L: reliable %v %v, want small", a.Draw.Litres, a.Reliable, a.Reasons)
		}
	}
}

// Every attribution in every fixture: shares in [0, 1] summing to 1 where
// known, unknown when masked (a reheat, or HW unknown), and a reliable
// balance only on a draw of MinReliableLitres or more with HW off. The
// right tank's shares on multi-litre draws are logged as a distribution.
//
// The band: on unmasked draws of 10 L or more, from the balance sweep, the
// right tank's share is 0.85–0.95 (the 50 L shower 0.89, the 12 L evening
// tap 0.90). A draw whose cold layer sat in a gap (thermocline_gap) is left
// out: the profile can't see that layer move, so its losses, and the split,
// are wrong (the 15 L straight after the shower reads 1.00). Below 10 L the
// split scatters from 0 to 1: the inlet probe's transient dip and the
// heat-sunk outlet probe warming to its true reading are as large as the
// draw.
func TestFixtureShareDistribution(t *testing.T) {
	cfg := testConfig()
	var shares []float64
	banded := 0
	for _, name := range fixtures {
		as, _ := replayFixture(t, name, cfg)
		for _, a := range as {
			if a.Reliable && (a.Draw.Litres < cfg.MinReliableLitres || a.MaskedByReheat) {
				t.Errorf("%s %v L: reliable %v", name, a.Draw.Litres, a.Reasons)
			}
			sum, known := 0.0, 0
			for _, x := range a.Tanks {
				if x.Share != nil {
					known++
					sum += *x.Share
					if *x.Share < 0 || *x.Share > 1 {
						t.Errorf("%s %v L: %s share %v", name, a.Draw.Litres, x.DeviceID, *x.Share)
					}
				}
			}
			if known > 0 && (known != len(a.Tanks) || sum < 1-1e-9 || sum > 1+1e-9) {
				t.Errorf("%s %v L: %d of %d shares known, summing to %v", name, a.Draw.Litres, known, len(a.Tanks), sum)
			}
			masked := a.MaskedByReheat || slices.Contains(a.Reasons, ReasonHWUnknown)
			if masked && known != 0 {
				t.Errorf("%s %v L: masked (%v) with shares", name, a.Draw.Litres, a.Reasons)
			}
			r := tankOf(t, a, right)
			if a.Draw.Litres >= 2 && r.Share != nil {
				shares = append(shares, *r.Share)
			}
			if a.Draw.Litres >= 10 && !masked {
				t.Logf("%s %v L: right share %s %v", name, a.Draw.Litres, fm(r.Share, "%.3f"), a.Reasons)
				if slices.Contains(a.Reasons, ReasonThermocline) {
					continue
				}
				if r.Share == nil || *r.Share < 0.85 || *r.Share > 0.95 {
					t.Errorf("%s %v L: right share %s, want 0.85–0.95", name, a.Draw.Litres, fm(r.Share, "%.3f"))
				}
				banded++
			}
		}
	}
	if banded < 2 {
		t.Errorf("only %d draws in the share band", banded)
	}
	sort.Float64s(shares)
	t.Logf("right share on %d multi-litre draws: %v", len(shares), shares)
	if len(shares) > 0 {
		t.Logf("min %.2f median %.2f max %.2f", shares[0], shares[len(shares)/2], shares[len(shares)-1])
	}
}
