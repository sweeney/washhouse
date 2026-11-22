package draw

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/tank"
	"github.com/sweeney/washhouse/internal/wire/boiler"
)

const (
	left  = "hot_water_cylinder_left"
	right = "hot_water_cylinder_right"
)

var t0 = time.Date(2001, 1, 1, 7, 0, 0, 0, time.UTC)

func f(x float64) *float64 { return &x }

// probe temperatures bottom to top: inlet 265, bulk 400, bulk 825, bulk 945,
// outlet 1215 mm. nil is a failed read.
type temps [5]*float64

func tt(p1, p2, p3, p4, p5 float64) temps { return temps{f(p1), f(p2), f(p3), f(p4), f(p5)} }

var heights = [5]float64{265, 400, 825, 945, 1215}
var roles = [5]profile.Role{profile.RoleInlet, profile.RoleBulk, profile.RoleBulk, profile.RoleBulk, profile.RoleOutlet}

// sw is one tank's sweep with stored energy kwh and the given probes.
func sw(id string, at time.Time, kwh float64, p temps) tank.Sweep {
	s := tank.Sweep{DeviceID: id, At: at, ClockSynced: true, ProbesTotal: 5,
		Estimate: estimate.Tank{KWh: kwh, TopC: 60}}
	for k := range p {
		s.Probes = append(s.Probes, tank.Probe{Position: string(rune('1' + k)), Role: roles[k], HeightMM: heights[k], TempC: p[k]})
		if p[k] != nil {
			s.ProbesOK++
		}
	}
	return s
}

// shower is a 50 L draw from t0+10 s to t0+8 min.
func shower() meter.Draw {
	return meter.Draw{ID: "20010101T060000Z-2", Start: t0.Add(10 * time.Second), End: t0.Add(8 * time.Minute),
		Duration: 470 * time.Second, Pulses: 50, Litres: 50, Kind: meter.KindShower, Ended: true}
}

// layered is a layered tank's probes, with the outlet reading out.
func layered(out float64) temps { return tt(40, 55, 61, 60, out) }

// showerWindow: each tank has a before sweep, two sweeps during the draw,
// a share sweep at End+60 s and a balance sweep at End+8 min, at energies
// before, share and balance.
func showerWindow(lb, ls, lbal, rb, rs, rbal float64) Window {
	d := shower()
	mk := func(id string, b, s, bal, peak float64) []tank.Sweep {
		return []tank.Sweep{
			sw(id, d.Start.Add(-5*time.Second), b, layered(55)),
			sw(id, d.Start.Add(2*time.Minute), b-0.3, tt(15, 55, 61, 60, peak-1)),
			sw(id, d.Start.Add(5*time.Minute), b-0.6, tt(12, 50, 61, 60, peak)),
			sw(id, d.End.Add(60*time.Second), s, tt(13, 40, 61, 60, peak-0.5)),
			sw(id, d.End.Add(8*time.Minute), bal, tt(20, 30, 61, 60, 58)),
		}
	}
	return Window{Sweeps: map[string][]tank.Sweep{
		left:  mk(left, lb, ls, lbal, 65),
		right: mk(right, rb, rs, rbal, 62),
	}}
}

func tankOf(t *testing.T, a Attribution, id string) TankAttribution {
	t.Helper()
	for _, x := range a.Tanks {
		if x.DeviceID == id {
			return x
		}
	}
	t.Fatalf("no tank %s in %+v", id, a)
	return TankAttribution{}
}

func near(got *float64, want, tol float64) bool { return got != nil && math.Abs(*got-want) <= tol }

func show(p *float64) any {
	if p == nil {
		return "nil"
	}
	return *p
}

const c = 4.186 / 3600

// Each tank's share is its heat lost over what a litre from its own outlet
// carries, normalised over the tanks. Shares, losses and outlet peaks come
// from the share sweep; the energy used and the heat balance from the
// balance sweep.
func TestSharesFromHeatLostOverOwnOutlet(t *testing.T) {
	cfg := testConfig()
	// left lost 0.6 kWh by the share sweep, right 1.8; by the balance
	// sweep 0.4 and 2.6.
	a := Attribute(cfg, shower(), showerWindow(11, 10.4, 10.6, 10.7, 8.9, 8.1))
	lL := 0.6 / (c * (65 - 10))
	rL := 1.8 / (c * (62 - 10))
	wantR := rL / (lL + rL)
	l, r := tankOf(t, a, left), tankOf(t, a, right)
	if !near(r.Share, wantR, 1e-9) || !near(l.Share, 1-wantR, 1e-9) {
		t.Fatalf("shares left %v right %v, want %v / %v", show(l.Share), show(r.Share), 1-wantR, wantR)
	}
	if !near(r.Litres, 50*wantR, 1e-9) || !near(l.Litres, 50*(1-wantR), 1e-9) {
		t.Errorf("litres left %v right %v", show(l.Litres), show(r.Litres))
	}
	if !near(l.LostKWh, 0.6, 1e-9) || !near(r.LostKWh, 1.8, 1e-9) {
		t.Errorf("lost left %v right %v, want 0.6 / 1.8 (share sweep)", show(l.LostKWh), show(r.LostKWh))
	}
	if !near(l.SettledLostKWh, 0.4, 1e-9) || !near(r.SettledLostKWh, 2.6, 1e-9) {
		t.Errorf("settled lost left %v right %v, want 0.4 / 2.6 (balance sweep)", show(l.SettledLostKWh), show(r.SettledLostKWh))
	}
	if !near(l.OutletPeakC, 65, 0) || !near(r.OutletPeakC, 62, 0) {
		t.Errorf("outlet peaks left %v right %v, want 65 / 62", show(l.OutletPeakC), show(r.OutletPeakC))
	}
	wantOut := (1-wantR)*65 + wantR*62
	if !near(a.OutletC, wantOut, 1e-9) {
		t.Errorf("system outlet %v, want the share-weighted peak %v", show(a.OutletC), wantOut)
	}
	if !near(a.EnergyKWh, 3.0, 1e-9) {
		t.Errorf("energy %v, want 3.0 (balance sweep)", show(a.EnergyKWh))
	}
	if !near(a.HeatBalanceL, 3.0/(c*(wantOut-10)), 1e-9) {
		t.Errorf("heat balance %v, want %v", show(a.HeatBalanceL), 3.0/(c*(wantOut-10)))
	}
	if !a.Reliable || len(a.Reasons) != 0 || a.MaskedByReheat {
		t.Errorf("reliable %v reasons %v masked %v, want a reliable balance", a.Reliable, a.Reasons, a.MaskedByReheat)
	}
}

// With the shares' own losses at the balance sweep, the heat balance is the
// sum of the tanks' litres: share-weighting the outlet peak makes the two
// agree.
func TestHeatBalanceIsSumOfTankLitres(t *testing.T) {
	cfg := testConfig()
	a := Attribute(cfg, shower(), showerWindow(11, 10.4, 10.4, 10.7, 8.9, 8.9))
	sum := 0.6/(c*55) + 1.8/(c*52)
	if !near(a.HeatBalanceL, sum, 1e-9) {
		t.Errorf("heat balance %v, want the tanks' litres summed %v", show(a.HeatBalanceL), sum)
	}
}

func TestSharesSumToOneAndOrderFollowsConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Tanks = []string{right, left}
	a := Attribute(cfg, shower(), showerWindow(11, 10.4, 10.6, 10.7, 8.9, 8.1))
	if len(a.Tanks) != 2 || a.Tanks[0].DeviceID != right || a.Tanks[1].DeviceID != left {
		t.Fatalf("tanks %+v, want the config's order", a.Tanks)
	}
	if s := *a.Tanks[0].Share + *a.Tanks[1].Share; math.Abs(s-1) > 1e-12 {
		t.Errorf("shares sum to %v", s)
	}
}

// A tank that gained heat by the share sweep supplied nothing measurable:
// its share is 0, never negative, and the other takes the draw.
func TestNegativeLossClampsToZeroShare(t *testing.T) {
	cfg := testConfig()
	a := Attribute(cfg, shower(), showerWindow(11, 11.2, 10.9, 10.7, 8.9, 8.1))
	if l, r := tankOf(t, a, left), tankOf(t, a, right); !near(l.Share, 0, 0) || !near(r.Share, 1, 0) {
		t.Errorf("shares left %v right %v, want 0 / 1", show(l.Share), show(r.Share))
	}
}

// No tank lost heat by the share sweep: the split is unknown, not 50/50.
func TestNoLossLeavesSharesUnknown(t *testing.T) {
	cfg := testConfig()
	a := Attribute(cfg, shower(), showerWindow(11, 11.1, 11, 10.7, 10.8, 10.7))
	for _, x := range a.Tanks {
		if x.Share != nil || x.Litres != nil {
			t.Errorf("%s share %v litres %v, want unknown", x.DeviceID, show(x.Share), show(x.Litres))
		}
	}
	if a.OutletC != nil || a.Reliable || !slices.Contains(a.Reasons, ReasonNoLoss) {
		t.Errorf("outlet %v reliable %v reasons %v, want unknown, unreliable, %q", show(a.OutletC), a.Reliable, a.Reasons, ReasonNoLoss)
	}
}

func reasonsOf(t *testing.T, a Attribution, want ...Reason) {
	t.Helper()
	if !slices.Equal(a.Reasons, want) || a.Reliable != (len(want) == 0) {
		t.Errorf("reasons %v reliable %v, want %v", a.Reasons, a.Reliable, want)
	}
}

func bigShower() Window { return showerWindow(11, 10.4, 10.6, 10.7, 8.9, 8.1) }

// HW on at any moment from the first before sweep to the last balance sweep
// masks the draw; HW off before the window, or on only after it, doesn't.
func TestHWOnMasks(t *testing.T) {
	cfg := testConfig()
	d := shower()
	for _, tc := range []struct {
		name   string
		hw     []Programmer
		masked bool
	}{
		{"unknown", nil, false},
		{"off throughout", []Programmer{{t0.Add(-time.Hour), boiler.Off}}, false},
		{"on throughout", []Programmer{{t0.Add(-time.Hour), boiler.On}}, true},
		{"on mid-draw", []Programmer{{t0.Add(-time.Hour), boiler.Off}, {d.Start.Add(time.Minute), boiler.On}}, true},
		{"on while settling", []Programmer{{t0.Add(-time.Hour), boiler.Off}, {d.End.Add(7 * time.Minute), boiler.On}}, true},
		{"a burst mid-window", []Programmer{{t0.Add(-time.Hour), boiler.Off}, {d.End, boiler.On}, {d.End.Add(time.Minute), boiler.Off}}, true},
		{"off before the window", []Programmer{{t0.Add(-time.Hour), boiler.On}, {d.Start.Add(-time.Minute), boiler.Off}}, false},
		{"on after the window", []Programmer{{t0.Add(-time.Hour), boiler.Off}, {d.End.Add(9 * time.Minute), boiler.On}}, false},
		{"on then unknown", []Programmer{{t0.Add(-time.Hour), boiler.On}, {d.Start.Add(-time.Minute), boiler.Unknown}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := bigShower()
			w.HW = tc.hw
			a := Attribute(cfg, d, w)
			if a.MaskedByReheat != tc.masked {
				t.Fatalf("masked %v, want %v", a.MaskedByReheat, tc.masked)
			}
			if tc.masked {
				reasonsOf(t, a, ReasonHWOn)
			} else {
				reasonsOf(t, a)
			}
		})
	}
}

// A tank whose energy rose by more than GainKWh from its before sweep to its
// balance sweep gained heat: masked. At exactly GainKWh it is not.
func TestTankGainMasks(t *testing.T) {
	cfg := testConfig()
	cfg.GainKWh = 0.25 // exact in binary, so the boundary is exact
	a := Attribute(cfg, shower(), showerWindow(11, 10.4, 11.375, 10.7, 8.9, 8.1))
	if !a.MaskedByReheat {
		t.Errorf("a gain above GainKWh isn't masked")
	}
	reasonsOf(t, a, ReasonGained)
	a = Attribute(cfg, shower(), showerWindow(11, 10.4, 11.25, 10.7, 8.9, 8.1))
	if a.MaskedByReheat {
		t.Errorf("a gain of exactly GainKWh masked the draw")
	}
	reasonsOf(t, a)
}

// Under MinReliableLitres the balance is unreliable; at it, reliable.
func TestSmallDrawBoundary(t *testing.T) {
	cfg := testConfig()
	d := shower()
	d.Litres, d.Pulses = 19, 19
	reasonsOf(t, Attribute(cfg, d, bigShower()), ReasonSmall)
	d.Litres, d.Pulses = 20, 20
	reasonsOf(t, Attribute(cfg, d, bigShower()))
}

// A tank whose GapLow probe reads below ThermoclineC at the before sweep
// while its GapHigh probe reads at or above it hides its cold layer. Either
// tank flags it; a tank cold at both, or hot at both, doesn't.
func TestThermoclineInGap(t *testing.T) {
	cfg := testConfig()
	for _, tc := range []struct {
		name   string
		before temps
		want   []Reason
	}{
		{"layered", tt(40, 55, 61, 60, 55), nil},
		{"cold layer in the gap", tt(14, 18, 60, 60, 55), []Reason{ReasonThermocline}},
		{"boundary: low at T_min", tt(14, 40, 60, 60, 55), nil},
		{"boundary: high at T_min", tt(14, 30, 40, 60, 55), []Reason{ReasonThermocline}},
		{"cold above the gap too", tt(14, 18, 35, 60, 55), nil},
		{"low probe unread", temps{f(14), nil, f(60), f(60), f(55)}, []Reason{ReasonGapUnread}},
		{"high probe unread", temps{f(14), f(50), nil, f(60), f(55)}, []Reason{ReasonGapUnread}},
	} {
		for _, id := range []string{left, right} {
			t.Run(tc.name+"/"+id, func(t *testing.T) {
				w := bigShower()
				w.Sweeps[id][0].Probes = sw(id, t0, 0, tc.before).Probes
				reasonsOf(t, Attribute(cfg, shower(), w), tc.want...)
			})
		}
	}
}

// A suspect reading at a gap probe is unread.
func TestThermoclineSuspectIsUnread(t *testing.T) {
	w := bigShower()
	w.Sweeps[right][0].Probes[1].Suspect = true
	reasonsOf(t, Attribute(testConfig(), shower(), w), ReasonGapUnread)
}

// The outlet peak is the highest outlet reading from the first litre to the
// share sweep: not the before sweep's, and not the re-warming after it.
func TestOutletPeakWindow(t *testing.T) {
	w := bigShower()
	w.Sweeps[left][0].Probes[4].TempC = f(70) // before the first litre
	w.Sweeps[left][4].Probes[4].TempC = f(72) // the balance sweep, re-warming
	w.Sweeps[left][3].Probes[4].TempC = f(66) // the share sweep counts
	a := Attribute(testConfig(), shower(), w)
	if l := tankOf(t, a, left); !near(l.OutletPeakC, 66, 0) || l.OutletFromProfile {
		t.Errorf("left peak %v (from profile %v), want 66", show(l.OutletPeakC), l.OutletFromProfile)
	}
}

// Without an outlet reading during the draw, the share uses the before
// sweep's profile top, and says so; the peak itself is unknown.
func TestMissingOutletFallsBackToProfileTop(t *testing.T) {
	w := bigShower()
	for i := range w.Sweeps[right] {
		w.Sweeps[right][i].Probes[4].TempC = nil
	}
	w.Sweeps[right][0].Estimate.TopC = 57
	a := Attribute(testConfig(), shower(), w)
	r := tankOf(t, a, right)
	if r.OutletPeakC != nil || !r.OutletFromProfile || !near(r.PeakUsedC, 57, 0) {
		t.Fatalf("right peak %v used %v from profile %v, want nil, 57, true", show(r.OutletPeakC), show(r.PeakUsedC), r.OutletFromProfile)
	}
	lL, rL := 0.6/(c*55), 1.8/(c*47)
	if !near(r.Share, rL/(lL+rL), 1e-9) {
		t.Errorf("right share %v, want %v", show(r.Share), rL/(lL+rL))
	}
	reasonsOf(t, a)
}

// The inlet drop is the inlet probe's fall from the before sweep to its
// lowest by the share sweep; the system's is the largest.
func TestInletDrop(t *testing.T) {
	a := Attribute(testConfig(), shower(), bigShower())
	// before 40, lowest 12 (the balance sweep's 20 doesn't count).
	if l := tankOf(t, a, left); !near(l.InletDropC, 28, 1e-9) {
		t.Errorf("left inlet drop %v, want 28", show(l.InletDropC))
	}
	w := bigShower()
	w.Sweeps[right][2].Probes[0].TempC = f(9)
	a = Attribute(testConfig(), shower(), w)
	if !near(a.InletDropC, 31, 1e-9) {
		t.Errorf("system inlet drop %v, want 31", show(a.InletDropC))
	}
	w.Sweeps[right][0].Probes[0].TempC = nil
	if r := tankOf(t, Attribute(testConfig(), shower(), w), right); r.InletDropC != nil {
		t.Errorf("inlet drop %v without a before reading, want nil", show(r.InletDropC))
	}
}

// A tank with no sweeps: its losses are unknown, and so is every share
// (normalising over the other would hand it the whole draw), the outlet
// and the balance. The reporting tank's own loss is still given.
func TestTankNotReporting(t *testing.T) {
	w := bigShower()
	delete(w.Sweeps, left)
	a := Attribute(testConfig(), shower(), w)
	l, r := tankOf(t, a, left), tankOf(t, a, right)
	if l.LostKWh != nil || l.Share != nil || r.Share != nil || a.OutletC != nil || a.HeatBalanceL != nil || a.EnergyKWh != nil {
		t.Errorf("left lost %v shares %v/%v outlet %v balance %v energy %v, want all unknown",
			show(l.LostKWh), show(l.Share), show(r.Share), show(a.OutletC), show(a.HeatBalanceL), show(a.EnergyKWh))
	}
	if !near(r.LostKWh, 1.8, 1e-9) {
		t.Errorf("right lost %v, want 1.8", show(r.LostKWh))
	}
	reasonsOf(t, a, ReasonTankMissing)
}

// Sweeps too far from their targets don't count: a before sweep older than
// MaxSweepGap, or a share or balance sweep more than MaxSweepGap late.
func TestStaleSweepsAreMissing(t *testing.T) {
	cfg := testConfig()
	d := shower()
	for _, tc := range []struct {
		name    string
		edit    func(ss []tank.Sweep)
		shares  bool
		balance bool
	}{
		{"before too old", func(ss []tank.Sweep) { ss[0].At = d.Start.Add(-cfg.MaxSweepGap - time.Second) }, false, false},
		{"before at the limit", func(ss []tank.Sweep) { ss[0].At = d.Start.Add(-cfg.MaxSweepGap) }, true, true},
		{"share sweep late", func(ss []tank.Sweep) { ss[3].At = d.End.Add(cfg.ShareSettle + cfg.MaxSweepGap + time.Second) }, false, false},
		{"balance sweep late", func(ss []tank.Sweep) { ss[4].At = d.End.Add(cfg.BalanceSettle + cfg.MaxSweepGap + time.Second) }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := bigShower()
			tc.edit(w.Sweeps[left])
			a := Attribute(cfg, d, w)
			if got := tankOf(t, a, right).Share != nil; got != tc.shares {
				t.Errorf("shares known %v, want %v", got, tc.shares)
			}
			if got := a.HeatBalanceL != nil; got != tc.balance {
				t.Errorf("balance known %v, want %v", got, tc.balance)
			}
			if tc.balance {
				reasonsOf(t, a)
			} else {
				reasonsOf(t, a, ReasonTankMissing)
			}
		})
	}
}

// The next draw starting before the balance sweep cuts the balance short:
// it uses each tank's last sweep before the next first litre, and is
// unreliable. A next draw after the balance sweep changes nothing.
func TestNextDrawInterruptsBalance(t *testing.T) {
	cfg := testConfig()
	d := shower()
	w := bigShower()
	// add a sweep at End+4 min for each tank
	for _, id := range []string{left, right} {
		ss := w.Sweeps[id]
		mid := sw(id, d.End.Add(4*time.Minute), ss[3].Estimate.KWh-0.1, tt(20, 35, 61, 60, 58))
		w.Sweeps[id] = append(ss[:4:4], mid, ss[4])
	}
	w.Cut = d.End.Add(5 * time.Minute)
	a := Attribute(cfg, d, w)
	reasonsOf(t, a, ReasonInterrupted)
	if l := tankOf(t, a, left); !l.BalanceAt.Equal(d.End.Add(4*time.Minute)) || !near(l.SettledLostKWh, 0.7, 1e-9) {
		t.Errorf("left balance at %v lost %v, want End+4m and 0.7", l.BalanceAt, show(l.SettledLostKWh))
	}
	if !near(a.EnergyKWh, 0.7+1.9, 1e-9) {
		t.Errorf("energy %v, want 2.6", show(a.EnergyKWh))
	}
	w.Cut = d.End.Add(cfg.BalanceSettle + time.Second)
	reasonsOf(t, Attribute(cfg, d, w))
}

// A next draw inside the share window spoils the split as well.
func TestNextDrawBeforeShareSweep(t *testing.T) {
	cfg := testConfig()
	cfg.ShareSettle = 2 * time.Minute
	d := shower()
	w := bigShower()
	for _, id := range []string{left, right} {
		w.Sweeps[id][3].At = d.End.Add(2 * time.Minute)
	}
	w.Cut = d.End.Add(90 * time.Second)
	a := Attribute(cfg, d, w)
	reasonsOf(t, a, ReasonInterrupted)
	if l := tankOf(t, a, left); !l.BalanceAt.Equal(l.ShareAt) {
		t.Errorf("balance at %v, want the share sweep %v", l.BalanceAt, l.ShareAt)
	}
}

// The balance clamps at zero when the tanks gained overall (and says so
// through the gain rule); it is never negative.
func TestBalanceNeverNegative(t *testing.T) {
	a := Attribute(testConfig(), shower(), showerWindow(11, 10.4, 11.5, 10.7, 8.9, 10.8))
	if !near(a.HeatBalanceL, 0, 0) {
		t.Errorf("balance %v, want 0", show(a.HeatBalanceL))
	}
	reasonsOf(t, a, ReasonGained)
}
