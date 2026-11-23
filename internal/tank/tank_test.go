package tank

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/common/wire/tanktemp"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/vessel"
)

var at = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func f(x float64) *float64 { return &x }
func i(x int) *int         { return &x }
func s(x string) *string   { return &x }

// testEstimator is avail.py's STI 210 vessel with the whole vessel drawable.
func testEstimator(t *testing.T) *estimate.Estimator {
	t.Helper()
	v, err := vessel.New(vessel.Params{CapacityL: 200, InnerDiameterMM: 460, DomeDepthMM: 60, Z0MM: 150, SliceMM: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := estimate.New(v, estimate.Config{ColdC: 10, MinC: 40, MixC: 40, KWhPerLitreK: 4.186 / 3600,
		LowFactor: 0.85, DrawFromMM: 0, DrawToMM: 2000})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// rightTank is the right cylinder's roles: P4 sits on the secondary return.
func rightTank() Config {
	return Config{
		DeviceID: "hot_water_cylinder_right",
		Probes: []ProbeConfig{
			{Position: "bottom", Role: profile.RoleInlet, HeightMM: 265},
			{Position: "lower", Role: profile.RoleBulk, HeightMM: 400},
			{Position: "middle", Role: profile.RoleBulk, HeightMM: 825},
			{Position: "upper", Role: profile.RoleReturn, HeightMM: 945},
			{Position: "top", Role: profile.RoleOutlet, HeightMM: 1215},
		},
	}
}

func state(temps ...*float64) tanktemp.State {
	pos := []string{"bottom", "lower", "middle", "upper", "top"}
	hts := []int{265, 400, 825, 945, 1215}
	st := tanktemp.State{DeviceID: "hot_water_cylinder_right", Timestamp: at, ClockSynced: true}
	for k, p := range pos {
		sn := tanktemp.Sensor{Position: p, Order: k + 1, HeightMM: i(hts[k]), TempC: temps[k], RawC: temps[k]}
		if temps[k] == nil {
			sn.Error = s("crc_mismatch")
		}
		st.Sensors = append(st.Sensors, sn)
	}
	return st
}

func mustTank(t *testing.T, c Config) *Tank {
	t.Helper()
	tk, err := New(c, testEstimator(t))
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestSweepAppliesRolesAndEstimates(t *testing.T) {
	tk := mustTank(t, rightTank())
	sw, err := tk.Sweep(state(f(20), f(45), f(60), f(52), f(50)))
	if err != nil {
		t.Fatal(err)
	}
	if sw.DeviceID != "hot_water_cylinder_right" || !sw.At.Equal(at) || sw.ProbesOK != 5 || sw.ProbesTotal != 5 {
		t.Fatalf("sweep %+v", sw)
	}
	roles := []profile.Role{profile.RoleInlet, profile.RoleBulk, profile.RoleBulk, profile.RoleReturn, profile.RoleOutlet}
	for k, p := range sw.Probes {
		if p.Role != roles[k] {
			t.Errorf("probe %d role %q, want %q", k+1, p.Role, roles[k])
		}
	}
	// Strat lifts the return probe (52 under 60) and the heat-sunk outlet.
	if got := sw.Profile.At(945); got != 60 {
		t.Errorf("profile at the return probe %v, want 60 (strat)", got)
	}
	want := testEstimator(t).Assess(mustProfile(t, 20, 45, 60, 52, 50))
	if sw.Estimate != want {
		t.Errorf("estimate %+v, want %+v", sw.Estimate, want)
	}
	if sw.Inlet() == nil || *sw.Inlet() != 20 || sw.Outlet() == nil || *sw.Outlet() != 50 {
		t.Errorf("inlet %v outlet %v", sw.Inlet(), sw.Outlet())
	}
}

func mustProfile(t *testing.T, temps ...float64) *profile.Profile {
	t.Helper()
	hts := []float64{265, 400, 825, 945, 1215}
	var ps []profile.Probe
	for k, x := range temps {
		ps = append(ps, profile.Probe{HeightMM: hts[k], TempC: f(x)})
	}
	p, err := profile.New(ps, profile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// tanktemp's own height wins; the config's is the fallback when the
// payload has none.
func TestHeightFromPayloadThenConfig(t *testing.T) {
	c := rightTank()
	c.Probes[2].HeightMM = 800 // config disagrees with the payload's 825
	tk := mustTank(t, c)
	st := state(f(20), f(45), f(60), f(52), f(50))
	sw, err := tk.Sweep(st)
	if err != nil {
		t.Fatal(err)
	}
	if sw.Probes[2].HeightMM != 825 {
		t.Errorf("height %v, want the payload's 825", sw.Probes[2].HeightMM)
	}
	st.Sensors[2].HeightMM = nil
	if sw, err = tk.Sweep(st); err != nil || sw.Probes[2].HeightMM != 800 {
		t.Errorf("height %v (%v), want the config's 800", sw.Probes[2].HeightMM, err)
	}
}

// A suspect reading is kept in the sweep but is unknown to the profile.
func TestSuspectReadingIsRecordedNotUsed(t *testing.T) {
	tk := mustTank(t, rightTank())
	st := state(f(85), f(40), f(60), f(60), f(60))
	st.Sensors[0].Suspect = s("85c_unverified")
	sw, err := tk.Sweep(st)
	if err != nil {
		t.Fatal(err)
	}
	if !sw.Probes[0].Suspect || sw.Probes[0].TempC == nil || *sw.Probes[0].TempC != 85 {
		t.Errorf("probe 1 %+v", sw.Probes[0])
	}
	if sw.ProbesOK != 4 {
		t.Errorf("ProbesOK %d, want 4 (the suspect one isn't OK)", sw.ProbesOK)
	}
	if got := sw.Profile.At(265); got != 40 {
		t.Errorf("profile at P1 %v, want 40 (flat below P2, the suspect 85 unknown)", got)
	}
}

func TestFailedProbeIsUnknown(t *testing.T) {
	tk := mustTank(t, rightTank())
	sw, err := tk.Sweep(state(f(20), nil, f(60), f(60), f(60)))
	if err != nil {
		t.Fatal(err)
	}
	if sw.ProbesOK != 4 || sw.Probes[1].TempC != nil {
		t.Errorf("sweep %+v", sw)
	}
	if got := sw.Profile.At(400); math.Abs(got-(20+40*135.0/560)) > 1e-9 {
		t.Errorf("profile at the failed P2 %v: want linear between P1 and P3", got)
	}
}

func TestTooFewProbesIsNoEstimate(t *testing.T) {
	tk := mustTank(t, rightTank())
	if _, err := tk.Sweep(state(nil, nil, nil, nil, f(60))); err == nil {
		t.Error("one usable probe gave an estimate")
	}
}

func TestSweepRefusesMismatches(t *testing.T) {
	tk := mustTank(t, rightTank())
	for name, mod := range map[string]func(*tanktemp.State){
		"other tank":       func(st *tanktemp.State) { st.DeviceID = "hot_water_cylinder_left" },
		"unknown position": func(st *tanktemp.State) { st.Sensors[1].Position = "sump" },
	} {
		st := state(f(20), f(45), f(60), f(52), f(50))
		mod(&st)
		if _, err := tk.Sweep(st); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	for name, mod := range map[string]func(*Config){
		"no device":          func(c *Config) { c.DeviceID = "" },
		"no probes":          func(c *Config) { c.Probes = nil },
		"duplicate position": func(c *Config) { c.Probes[1].Position = "bottom" },
		"unknown role":       func(c *Config) { c.Probes[0].Role = "sump" },
		"bad height":         func(c *Config) { c.Probes[0].HeightMM = math.NaN() },
		"negative height":    func(c *Config) { c.Probes[0].HeightMM = -1 },
	} {
		c := rightTank()
		mod(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		} else if !strings.HasPrefix(err.Error(), "tank: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := rightTank().Validate(); err != nil {
		t.Error(err)
	}
	if _, err := New(rightTank(), nil); err == nil {
		t.Error("New accepted a nil estimator")
	}
}

// A probe with no height in the payload and none configured (0) can't be
// placed: the sweep is refused rather than guessed.
func TestNoHeightAnywhereIsRefused(t *testing.T) {
	c := rightTank()
	c.Probes[0].HeightMM = 0
	tk := mustTank(t, c)
	st := state(f(20), f(45), f(60), f(52), f(50))
	if _, err := tk.Sweep(st); err != nil {
		t.Fatalf("payload height present: %v", err)
	}
	st.Sensors[0].HeightMM = nil
	if _, err := tk.Sweep(st); err == nil {
		t.Error("a probe with no height was placed")
	}
}

// BulkKWh is the stored energy seen by the bulk (and return) probes only:
// the inlet and outlet probes sit on fittings that react to a sub-litre
// draw, and the heat-sunk outlet reads 3–8 °C low at rest but true during
// flow, which the full estimate would read as heat gained.
func TestBulkEnergyLeavesOutTheFittings(t *testing.T) {
	tk := mustTank(t, rightTank())
	rest, err := tk.Sweep(state(f(20), f(45), f(60), f(52), f(50)))
	if err != nil {
		t.Fatal(err)
	}
	flowing, err := tk.Sweep(state(f(20), f(45), f(60), f(52), f(62))) // a tap runs: the outlet reads true
	if err != nil {
		t.Fatal(err)
	}
	dipped, err := tk.Sweep(state(f(14), f(45), f(60), f(52), f(62))) // and the inlet fitting dips
	if err != nil {
		t.Fatal(err)
	}
	if *dipped.BulkKWh != *rest.BulkKWh {
		t.Errorf("bulk energy moved with the inlet: %v → %v", *rest.BulkKWh, *dipped.BulkKWh)
	}
	if rest.BulkKWh == nil || flowing.BulkKWh == nil {
		t.Fatalf("bulk energy unknown: %v %v", rest.BulkKWh, flowing.BulkKWh)
	}
	if *rest.BulkKWh != *flowing.BulkKWh {
		t.Errorf("bulk energy moved with the fittings: %v → %v", *rest.BulkKWh, *flowing.BulkKWh)
	}
	if flowing.Estimate.KWh <= rest.Estimate.KWh {
		t.Errorf("the full estimate should rise with the outlet (that's the artefact): %v → %v", rest.Estimate.KWh, flowing.Estimate.KWh)
	}
	want := testEstimator(t).Assess(mustProfile3(t, 45, 60, 52)).KWh
	if math.Abs(*rest.BulkKWh-want) > 1e-12 {
		t.Errorf("bulk %v, want %v (profile from P2–P4)", *rest.BulkKWh, want)
	}
	// Fewer than two bulk probes: unknown, not zero.
	sw, err := tk.Sweep(state(f(20), nil, nil, f(52), f(50)))
	if err != nil {
		t.Fatal(err)
	}
	if sw.BulkKWh != nil {
		t.Errorf("one bulk probe gave %v", *sw.BulkKWh)
	}
}

func mustProfile3(t *testing.T, p2, p3, p4 float64) *profile.Profile {
	t.Helper()
	p, err := profile.New([]profile.Probe{{HeightMM: 400, TempC: f(p2)}, {HeightMM: 825, TempC: f(p3)}, {HeightMM: 945, TempC: f(p4)}}, profile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
