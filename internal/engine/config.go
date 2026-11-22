package engine

import (
	"errors"
	"fmt"
	"math"
	"slices"

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
	// Draw.Tanks must name the same cylinders in the same order.
	Tanks   []tank.Config
	Thermal thermal.Config
	Draw    draw.Config
	// LowHotWaterL: the system is low on hot water when the tanks'
	// usable_l40_low, summed, is below this (litres of 40 °C water).
	LowHotWaterL float64
	// RecentDraws: how many ended draws Recent keeps.
	RecentDraws int
}

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
	if c.Draw.ColdC != c.Estimate.ColdC {
		add(fmt.Errorf("engine: draw cold %v °C must be the estimate's %v °C", c.Draw.ColdC, c.Estimate.ColdC))
	}
	if c.Draw.KWhPerLitreK != c.Estimate.KWhPerLitreK {
		add(fmt.Errorf("engine: draw heat capacity %v must be the estimate's %v kWh per L·K", c.Draw.KWhPerLitreK, c.Estimate.KWhPerLitreK))
	}
	if math.IsNaN(c.LowHotWaterL) || math.IsInf(c.LowHotWaterL, 0) || c.LowHotWaterL < 0 {
		add(fmt.Errorf("engine: LowHotWaterL %v must be finite and at or above 0", c.LowHotWaterL))
	}
	if c.RecentDraws < 1 {
		add(fmt.Errorf("engine: RecentDraws %d must be at least 1", c.RecentDraws))
	}
	return errors.Join(errs...)
}
