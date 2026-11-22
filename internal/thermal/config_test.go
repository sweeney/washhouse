package thermal

import (
	"math"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/profile"
)

func TestConfigValidate(t *testing.T) {
	if err := planConfig().Validate(); err != nil {
		t.Fatalf("plan config: %v", err)
	}
	bad := map[string]func(*Config){
		"PowerWindow zero":          func(c *Config) { c.PowerWindow = 0 },
		"PowerMinSpan zero":         func(c *Config) { c.PowerMinSpan = 0 },
		"PowerMinSpan over window":  func(c *Config) { c.PowerMinSpan = c.PowerWindow + time.Second },
		"PowerMinSamples 2":         func(c *Config) { c.PowerMinSamples = 2 },
		"PowerExclude unknown role": func(c *Config) { c.PowerExclude = []profile.Role{"pipe"} },
		"MinProbes 1":               func(c *Config) { c.MinProbes = 1 },
		"StaleAfter zero":           func(c *Config) { c.StaleAfter = 0 },
		"HeatingOnKW zero":          func(c *Config) { c.HeatingOnKW = 0 },
		"HeatingOnKW NaN":           func(c *Config) { c.HeatingOnKW = math.NaN() },
		"HeatingOnKW Inf":           func(c *Config) { c.HeatingOnKW = math.Inf(1) },
		"HeatingOffKW above on":     func(c *Config) { c.HeatingOffKW = c.HeatingOnKW + 0.1 },
		"HeatingOffKW NaN":          func(c *Config) { c.HeatingOffKW = math.NaN() },
		"HeatingOffKW -Inf":         func(c *Config) { c.HeatingOffKW = math.Inf(-1) },
		"DrawSettle zero":           func(c *Config) { c.DrawSettle = 0 },
		"SourceHold zero":           func(c *Config) { c.SourceHold = 0 },
		"UATau zero":                func(c *Config) { c.UATau = 0 },
		"UAQuiet zero":              func(c *Config) { c.UAQuiet = 0 },
		"UAQuiet under window":      func(c *Config) { c.UAQuiet = c.PowerWindow - time.Second },
		"UAMinDeltaK zero":          func(c *Config) { c.UAMinDeltaK = 0 },
		"UAMinDeltaK NaN":           func(c *Config) { c.UAMinDeltaK = math.NaN() },
		"AmbientStale zero":         func(c *Config) { c.AmbientStale = 0 },
		"no tanks":                  func(c *Config) { c.Tanks = nil },
		"tank id empty":             func(c *Config) { c.Tanks[0].DeviceID = "" },
		"tank twice":                func(c *Config) { c.Tanks[1].DeviceID = c.Tanks[0].DeviceID },
		"prior UA zero":             func(c *Config) { c.Tanks[0].PriorUAWPerK = 0 },
		"prior UA NaN":              func(c *Config) { c.Tanks[0].PriorUAWPerK = math.NaN() },
		"prior hours zero":          func(c *Config) { c.Tanks[1].PriorHours = 0 },
		"prior hours Inf":           func(c *Config) { c.Tanks[1].PriorHours = math.Inf(1) },
	}
	for name, mut := range bad {
		c := planConfig()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
		if _, err := New(c, testEstimator(t)); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	// Off may equal on (no hysteresis), and the excluded roles may be none,
	// in which case power is the sweep's own estimate and no estimator is
	// needed.
	c := planConfig()
	c.HeatingOffKW = c.HeatingOnKW
	c.PowerExclude = nil
	if _, err := New(c, nil); err != nil {
		t.Errorf("off = on, no exclusions, no estimator: %v", err)
	}
	c = planConfig()
	if _, err := New(c, nil); err == nil {
		t.Error("exclusions without an estimator: accepted")
	}
}
