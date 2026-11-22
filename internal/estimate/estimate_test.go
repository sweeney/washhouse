package estimate

import (
	"math"
	"strings"
	"testing"

	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/vessel"
)

// testConfig is avail.py's: mains 10 °C, usable from 40 °C, mixed to 40 °C,
// 4.186 kJ/(L·K), and 85 % of plug-flow mix40 as the low estimate.
func testConfig() Config {
	return Config{ColdC: 10, MinC: 40, MixC: 40, KWhPerLitreK: 4.186 / 3600, LowFactor: 0.85}
}

func mustEstimator(t *testing.T, c Config) *Estimator {
	t.Helper()
	v, err := vessel.New(vessel.STI210())
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(v, c)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func prof(t *testing.T, raw bool, temps ...float64) *profile.Profile {
	t.Helper()
	hts := []float64{265, 400, 825, 945, 1215}
	ps := make([]profile.Probe, len(temps))
	for i := range temps {
		ps[i] = profile.Probe{HeightMM: hts[i], TempC: &temps[i]}
	}
	p, err := profile.New(ps, profile.Options{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v ± %v", name, got, want, tol)
	}
}

func TestUniformHotTank(t *testing.T) {
	e := mustEstimator(t, testConfig())
	got := e.Assess(prof(t, false, 60, 60, 60, 60, 60))
	near(t, "KWh", got.KWh, 200*50*4.186/3600, 1e-5)
	near(t, "HotL", got.HotL, 200, 1e-5)
	near(t, "UsableL40", got.UsableL40, 200*50.0/30, 1e-5)
	near(t, "UsableL40Low", got.UsableL40Low, 0.85*200*50.0/30, 1e-5)
	near(t, "MeanC", got.MeanC, 60, 1e-5)
	near(t, "TopC", got.TopC, 60, 0)
}

func TestTankBelowTMinHasNoUsableWater(t *testing.T) {
	e := mustEstimator(t, testConfig())
	got := e.Assess(prof(t, false, 39, 39, 39, 39, 39))
	near(t, "KWh", got.KWh, 200*29*4.186/3600, 1e-5)
	near(t, "HotL", got.HotL, 0, 0)
	near(t, "UsableL40", got.UsableL40, 0, 0)
	near(t, "MeanC", got.MeanC, 39, 1e-5)
}

func TestColdTankStoresNothing(t *testing.T) {
	e := mustEstimator(t, testConfig())
	got := e.Assess(prof(t, false, 10, 10, 10, 10, 10))
	near(t, "KWh", got.KWh, 0, 0)
	c := testConfig()
	c.ColdC = 5
	got = mustEstimator(t, c).Assess(prof(t, false, 10, 10, 10, 10, 10))
	near(t, "KWh with 5 °C mains", got.KWh, 200*5*4.186/3600, 1e-5)
}

func TestOnlyWaterAtOrAboveTMinCounts(t *testing.T) {
	// Hot (60 °C) at and above 825 mm, cold (10 °C) at and below 400 mm.
	e := mustEstimator(t, testConfig())
	got := e.Assess(prof(t, true, 10, 10, 60, 60, 60))
	// Between 400 and 825 mm the profile crosses 40 °C at 400 + 425*30/50 = 655 mm.
	v, _ := vessel.New(vessel.STI210())
	want := v.LitresBetween(655, 2000)
	near(t, "HotL", got.HotL, want, 0.02) // within one 1 mm slice
	if got.UsableL40 <= got.HotL {
		t.Errorf("UsableL40 %v should exceed HotL %v when the hot water is above 40 °C", got.UsableL40, got.HotL)
	}
}

func TestTopIsTheProfileAtTheTopOfTheWater(t *testing.T) {
	e := mustEstimator(t, testConfig())
	near(t, "strat TopC", e.Assess(prof(t, false, 40, 58, 60, 60, 49)).TopC, 60, 0)
	near(t, "raw TopC", e.Assess(prof(t, true, 40, 58, 60, 60, 49)).TopC, 49, 0)
}

func TestLowFactorIsConfig(t *testing.T) {
	c := testConfig()
	c.LowFactor = 0.5
	got := mustEstimator(t, c).Assess(prof(t, false, 60, 60, 60, 60, 60))
	near(t, "UsableL40Low", got.UsableL40Low, got.UsableL40*0.5, 1e-12)
}

func TestConfigValidate(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	cases := []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"NaN cold", func(c *Config) { c.ColdC = math.NaN() }, "cold"},
		{"Inf min", func(c *Config) { c.MinC = math.Inf(1) }, "min"},
		{"mix not above cold", func(c *Config) { c.MixC = 10 }, "mix"},
		{"min below mix", func(c *Config) { c.MinC = 39 }, "min"},
		{"zero heat capacity", func(c *Config) { c.KWhPerLitreK = 0 }, "kwh per litre"},
		{"zero low factor", func(c *Config) { c.LowFactor = 0 }, "low factor"},
		{"low factor above 1", func(c *Config) { c.LowFactor = 1.01 }, "low factor"},
	}
	v, _ := vessel.New(vessel.STI210())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			tc.mod(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error mentioning %q", err, tc.want)
			}
			if _, err := New(v, c); err == nil {
				t.Fatal("New accepted a config Validate refuses")
			}
		})
	}
	if _, err := New(nil, testConfig()); err == nil {
		t.Error("New accepted a nil vessel")
	}
}
