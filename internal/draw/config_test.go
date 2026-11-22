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
		GapLowMM:          400,
		GapHighMM:         825,
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
		{"gap inverted", func(c *Config) { c.GapLowMM, c.GapHighMM = 825, 400 }, "Gap"},
		{"gap zero", func(c *Config) { c.GapLowMM = 0 }, "Gap"},
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
