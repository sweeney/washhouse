package engine

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/tank"
	"github.com/sweeney/washhouse/internal/thermal"
	"github.com/sweeney/washhouse/internal/vessel"
)

// Config is everything the engine runs with: each package's own config,
// and the engine's few values. It has no defaults; Validate refuses what
// is missing or inconsistent.
type Config struct {
	Meter    meter.Config
	Vessel   vessel.Params // every cylinder's shape (the house's are the same model)
	Estimate estimate.Config
	// Tanks: each cylinder's probes. Their order is the order the state,
	// and every draw's attribution, list them; Thermal.Tanks and
	// Draw.Tanks must name the same cylinders in the same order, and
	// Draw.OutletSplit must be OutletSplit.
	Tanks   []tank.Config
	Thermal thermal.Config
	Draw    draw.Config
	// OutletSplit: the fraction of every litre drawn that each tank gives,
	// in Tanks order, summing to 1. The tanks are in parallel with one
	// merged outlet, so the pipework fixes the split whatever the tanks'
	// temperatures (an assumption: buoyancy could shift it towards a hotter
	// tank; no fixture has a draw with one tank cold). The system's usable
	// litres are a draw simulated at this split (estimate.Merged).
	OutletSplit []float64
	// LowHotWaterL and LowHotWaterMarginL: low_hot_water sets when the
	// system's usable_l40_low falls below LowHotWaterL (litres of 40 °C
	// water) and clears when it rises above LowHotWaterL +
	// LowHotWaterMarginL, so it doesn't flip on every sweep near the line.
	LowHotWaterL       float64
	LowHotWaterMarginL float64
	// BoilerHeartbeat: boiler-sensor's heartbeat interval, used when a
	// heartbeat doesn't carry its own (config.heartbeat_ms).
	// BoilerMissedHeartbeats: HW and CH are unknown once event time is more
	// than this many intervals past the last heartbeat or event (2.5).
	BoilerHeartbeat        time.Duration
	BoilerMissedHeartbeats float64
	// RecentDraws: how many ended draws Recent keeps.
	RecentDraws int
}

// deviceID is a device id: its key in devices_<site>, and a segment of
// washhouse's tank topic (docs/WIRE.md).
var deviceID = regexp.MustCompile(`^[a-z0-9_-]+$`)

// Validate reports the problems with c, or nil.
func (c Config) Validate() error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(c.Meter.Validate())
	add(c.Vessel.Validate())
	add(c.Estimate.Validate())
	if len(c.Tanks) == 0 {
		add(errors.New("engine: no tanks configured"))
	}
	var ids []string
	for _, tc := range c.Tanks {
		add(tc.Validate())
		if tc.DeviceID != "" && !deviceID.MatchString(tc.DeviceID) {
			add(fmt.Errorf("engine: tank %q: a device id must match [a-z0-9_-]+, since it is a topic segment", tc.DeviceID))
		}
		if slices.Contains(ids, tc.DeviceID) {
			add(fmt.Errorf("engine: tank %q configured twice", tc.DeviceID))
		}
		ids = append(ids, tc.DeviceID)
	}
	add(c.Thermal.Validate())
	var thermalIDs []string
	for _, tc := range c.Thermal.Tanks {
		thermalIDs = append(thermalIDs, tc.DeviceID)
	}
	if !slices.Equal(thermalIDs, ids) {
		add(fmt.Errorf("engine: thermal tanks %v must be the tanks %v, in order", thermalIDs, ids))
	}
	add(c.Draw.Validate())
	if !slices.Equal(c.Draw.Tanks, ids) {
		add(fmt.Errorf("engine: draw tanks %v must be the tanks %v, in order", c.Draw.Tanks, ids))
	}
	// The estimate and the attribution count heat alike.
	if !slices.Equal(c.Draw.OutletSplit, c.OutletSplit) {
		add(fmt.Errorf("engine: draw outlet split %v must be the outlet split %v", c.Draw.OutletSplit, c.OutletSplit))
	}
	if c.Draw.ColdC != c.Estimate.ColdC {
		add(fmt.Errorf("engine: draw cold %v °C must be the estimate's %v °C", c.Draw.ColdC, c.Estimate.ColdC))
	}
	if c.Draw.KWhPerLitreK != c.Estimate.KWhPerLitreK {
		add(fmt.Errorf("engine: draw heat capacity %v must be the estimate's %v kWh per L·K", c.Draw.KWhPerLitreK, c.Estimate.KWhPerLitreK))
	}
	// Every tank has both probes of every thermocline gap, the low one
	// below the high one where the config gives their heights.
	for _, g := range c.Draw.Gaps {
		for _, tc := range c.Tanks {
			lo, okLo := probeAt(tc, g.Low)
			hi, okHi := probeAt(tc, g.High)
			for _, x := range []struct {
				pos string
				ok  bool
			}{{g.Low, okLo}, {g.High, okHi}} {
				if !x.ok {
					add(fmt.Errorf("engine: tank %q has no %q probe for the thermocline gap %s–%s", tc.DeviceID, x.pos, g.Low, g.High))
				}
			}
			if okLo && okHi && lo.HeightMM > 0 && hi.HeightMM > 0 && !(lo.HeightMM < hi.HeightMM) {
				add(fmt.Errorf("engine: tank %q's gap probe %q (%v mm) must be below %q (%v mm)", tc.DeviceID, g.Low, lo.HeightMM, g.High, hi.HeightMM))
			}
		}
	}
	if len(c.OutletSplit) != len(c.Tanks) {
		add(fmt.Errorf("engine: OutletSplit has %d fractions for %d tanks", len(c.OutletSplit), len(c.Tanks)))
	} else {
		sum := 0.0
		for _, x := range c.OutletSplit {
			if math.IsNaN(x) || x < 0 || x > 1 {
				add(fmt.Errorf("engine: OutletSplit %v: each fraction must be in [0, 1]", c.OutletSplit))
			}
			sum += x
		}
		if math.Abs(sum-1) > 1e-9 {
			add(fmt.Errorf("engine: OutletSplit %v sums to %v, not 1", c.OutletSplit, sum))
		}
	}
	if math.IsNaN(c.LowHotWaterL) || math.IsInf(c.LowHotWaterL, 0) || c.LowHotWaterL < 0 {
		add(fmt.Errorf("engine: LowHotWaterL %v must be finite and at or above 0", c.LowHotWaterL))
	}
	if math.IsNaN(c.LowHotWaterMarginL) || math.IsInf(c.LowHotWaterMarginL, 0) || c.LowHotWaterMarginL < 0 {
		add(fmt.Errorf("engine: LowHotWaterMarginL %v must be finite and at or above 0", c.LowHotWaterMarginL))
	}
	if c.BoilerHeartbeat <= 0 {
		add(fmt.Errorf("engine: BoilerHeartbeat %v must be above 0", c.BoilerHeartbeat))
	}
	if math.IsNaN(c.BoilerMissedHeartbeats) || math.IsInf(c.BoilerMissedHeartbeats, 0) || !(c.BoilerMissedHeartbeats > 1) {
		add(fmt.Errorf("engine: BoilerMissedHeartbeats %v must be finite and above 1", c.BoilerMissedHeartbeats))
	}
	if c.RecentDraws < 1 {
		add(fmt.Errorf("engine: RecentDraws %d must be at least 1", c.RecentDraws))
	}
	return errors.Join(errs...)
}

// probeAt is tank tc's probe at position.
func probeAt(tc tank.Config, position string) (tank.ProbeConfig, bool) {
	for _, p := range tc.Probes {
		if p.Position == position {
			return p, true
		}
	}
	return tank.ProbeConfig{}, false
}
