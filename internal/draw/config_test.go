package draw

import (
	"math"
	"strings"
	"testing"
	"time"
)

// testConfig is the configuration the fixture tests settled on (see the
// package doc for why each value).
func testConfig() Config {
	return Config{
		Tanks:             []string{"hot_water_cylinder_left", "hot_water_cylinder_right"},
		ColdC:             10,
		KWhPerLitreK:      4.186 / 3600,
		ShareSettle:       60 * time.Second,
		BalanceSettle:     8 * time.Minute,
		MaxSweepGap:       2 * time.Minute,
		MinReliableLitres: 20,
		GainKWh:           0.05,
		ThermoclineC:      40,
		Gaps:              []Gap{{Low: "lower", High: "middle"}},
		OutletSplit:       []float64{0.11, 0.89},
	}
}

func TestConfigValidates(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"no tanks", func(c *Config) { c.Tanks = nil }, "Tanks"},
		{"empty tank", func(c *Config) { c.Tanks = []string{"a", ""} }, "Tanks"},
		{"no outlet split", func(c *Config) { c.OutletSplit = nil }, "OutletSplit"},
		{"split per tank", func(c *Config) { c.OutletSplit = []float64{1} }, "OutletSplit"},
		{"split sums to 1", func(c *Config) { c.OutletSplit = []float64{0.2, 0.7} }, "OutletSplit"},
		{"split in [0, 1]", func(c *Config) { c.OutletSplit = []float64{-0.5, 1.5} }, "OutletSplit"},
		{"duplicate tank", func(c *Config) { c.Tanks = []string{"a", "a"} }, "twice"},
		{"cold NaN", func(c *Config) { c.ColdC = math.NaN() }, "ColdC"},
		{"heat capacity zero", func(c *Config) { c.KWhPerLitreK = 0 }, "KWhPerLitreK"},
		{"share settle zero", func(c *Config) { c.ShareSettle = 0 }, "ShareSettle"},
		{"balance before share", func(c *Config) { c.BalanceSettle = 30 * time.Second }, "BalanceSettle"},
		{"sweep gap zero", func(c *Config) { c.MaxSweepGap = 0 }, "MaxSweepGap"},
		{"min litres zero", func(c *Config) { c.MinReliableLitres = 0 }, "MinReliableLitres"},
		{"gain negative", func(c *Config) { c.GainKWh = -0.1 }, "GainKWh"},
		{"gain inf", func(c *Config) { c.GainKWh = math.Inf(1) }, "GainKWh"},
		{"thermocline at cold", func(c *Config) { c.ThermoclineC = 10 }, "ThermoclineC"},
		{"no gaps", func(c *Config) { c.Gaps = nil }, "Gaps"},
		{"gap position empty", func(c *Config) { c.Gaps = []Gap{{Low: "", High: "middle"}} }, "Gaps"},
		{"gap one probe", func(c *Config) { c.Gaps = []Gap{{Low: "lower", High: "lower"}} }, "Gaps"},
		{"gap twice", func(c *Config) { c.Gaps = append(c.Gaps, c.Gaps[0]) }, "twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			tc.edit(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}
