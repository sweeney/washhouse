package engine

import (
	"math"
	"strings"
	"testing"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/tank"
	"github.com/sweeney/washhouse/internal/thermal"
)

func TestPlanConfigValidates(t *testing.T) {
	if err := planConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

// Validate delegates to every package's config, and checks what ties them
// together.
func TestConfigRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"meter", func(c *Config) { c.Meter.DrawEnd = 0 }, "DrawEnd"},
		{"vessel", func(c *Config) { c.Vessel.CapacityL = 0 }, "capacity"},
		{"estimate", func(c *Config) { c.Estimate.LowFactor = 0 }, "low factor"},
		{"no tanks", func(c *Config) { c.Tanks = nil }, "no tanks"},
		{"tank", func(c *Config) { c.Tanks[0].Probes = nil }, "no probes"},
		{"tank twice", func(c *Config) { c.Tanks[1] = c.Tanks[0] }, "twice"},
		{"thermal", func(c *Config) { c.Thermal.HeatingOnKW = 0 }, "heating on"},
		{"thermal tanks", func(c *Config) {
			c.Thermal.Tanks = []thermal.TankConfig{c.Thermal.Tanks[0]}
		}, "thermal tanks"},
		{"thermal tanks order", func(c *Config) {
			c.Thermal.Tanks[0], c.Thermal.Tanks[1] = c.Thermal.Tanks[1], c.Thermal.Tanks[0]
		}, "thermal tanks"},
		{"draw", func(c *Config) { c.Draw.ShareSettle = 0 }, "ShareSettle"},
		{"draw tanks", func(c *Config) { c.Draw.Tanks = []string{right, left} }, "draw tanks"},
		{"draw cold", func(c *Config) { c.Draw.ColdC = 12 }, "cold"},
		{"draw heat capacity", func(c *Config) { c.Draw.KWhPerLitreK = 0.001 }, "heat capacity"},
		{"low hot water negative", func(c *Config) { c.LowHotWaterL = -1 }, "LowHotWaterL"},
		{"low hot water NaN", func(c *Config) { c.LowHotWaterL = math.NaN() }, "LowHotWaterL"},
		{"low hot water inf", func(c *Config) { c.LowHotWaterL = math.Inf(1) }, "LowHotWaterL"},
		{"recent draws", func(c *Config) { c.RecentDraws = 0 }, "RecentDraws"},
		{"split length", func(c *Config) { c.OutletSplit = []float64{1} }, "OutletSplit"},
		{"split negative", func(c *Config) { c.OutletSplit = []float64{1.2, -0.2} }, "OutletSplit"},
		{"split NaN", func(c *Config) { c.OutletSplit = []float64{math.NaN(), 1} }, "OutletSplit"},
		{"split sum", func(c *Config) { c.OutletSplit = []float64{0.2, 0.7} }, "OutletSplit"},
		{"margin negative", func(c *Config) { c.LowHotWaterMarginL = -1 }, "LowHotWaterMarginL"},
		{"margin inf", func(c *Config) { c.LowHotWaterMarginL = math.Inf(1) }, "LowHotWaterMarginL"},
		{"heartbeat zero", func(c *Config) { c.BoilerHeartbeat = 0 }, "BoilerHeartbeat"},
		{"missed heartbeats one", func(c *Config) { c.BoilerMissedHeartbeats = 1 }, "BoilerMissedHeartbeats"},
		{"missed heartbeats NaN", func(c *Config) { c.BoilerMissedHeartbeats = math.NaN() }, "BoilerMissedHeartbeats"},
		{"gap position missing", func(c *Config) { c.Draw.Gaps = []draw.Gap{{Low: "lower", High: "attic"}} }, "attic"},
		{"gap position missing on one tank", func(c *Config) {
			c.Tanks[1].Probes = append([]tank.ProbeConfig(nil), c.Tanks[1].Probes...)
			c.Tanks[1].Probes[1].Position = "low"
		}, "lower"},
		{"gap upside down", func(c *Config) { c.Draw.Gaps = []draw.Gap{{Low: "middle", High: "lower"}} }, "below"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := planConfig()
			tc.edit(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
			if _, err := New(c); err == nil {
				t.Fatal("New accepted it")
			}
		})
	}
}
