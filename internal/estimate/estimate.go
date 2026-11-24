// Package estimate says how much hot water a tank holds, from its vessel
// model and temperature profile: stored energy above mains temperature,
// litres at or above the usable temperature, and how many litres of mixed
// water those make. It is a port of assess() in tanktemp's avail.py, and the
// parity test pins it to that script's output.
//
// Plug flow is assumed: the top water leaves first and none of it mixes
// with the cold water entering below. Real draws mix the thermocline, so
// UsableL40Low applies a configured factor (about 0.85) to UsableL40.
//
// Only water between DrawFromMM and DrawToMM is drawable: plug flow can't
// push out water below the cold inlet. The hot outlet has an internal pipe
// up to the top of the dome (the owner, 24 Nov 2026), so the band runs to
// the top of the water: 281 to 1412 mm on the STI 210, with 17 L below it.
// avail.py counted the whole vessel; its parity test sets the band to it.
//
// ColdC is configured, and 10 °C matches the inlet probes' minimum during
// long draws in Nov 2026, but mains water cools through winter. The plan is
// to measure it: probe 1's minimum during a long metered draw.
//
// kWh here comes from the strat profile over the vessel's slices (avail.py).
// tanktemp's own kWh reports take the top layer from probe 4 instead of the
// heat-sunk probe 5, so the two differ by a few tenths of a kWh.
//
// TODO(phase 2): TopC is the profile's value at the top of the water. The
// plan is for it to use the outlet probe's last draw peak, decayed; that
// enters through the outlet probe's reading when the profile is built (see
// package profile), so Assess needs no new input.
package estimate

import (
	"errors"
	"fmt"
	"math"

	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/vessel"
)

// Config holds the estimate's thresholds. Temperatures are in °C.
type Config struct {
	ColdC        float64 // mains (cold inlet) temperature; energy is counted above it
	MinC         float64 // water at or above this is usable; at least MixC
	MixC         float64 // delivered temperature hot water is mixed down to
	KWhPerLitreK float64 // heat capacity of water, kWh per litre per kelvin (4.186/3600)
	LowFactor    float64 // UsableL40Low = UsableL40 × LowFactor, in (0, 1]
	DrawFromMM   float64 // drawable water starts here (the cold inlet)
	DrawToMM     float64 // and ends here (the hot outlet, unless it has a dip pipe)
}

// Validate reports the problems with c, or nil.
func (c Config) Validate() error {
	var errs []error
	for _, f := range []struct {
		name string
		v    float64
	}{
		{"cold", c.ColdC}, {"min", c.MinC}, {"mix", c.MixC},
		{"kwh per litre per K", c.KWhPerLitreK}, {"low factor", c.LowFactor},
		{"draw from", c.DrawFromMM}, {"draw to", c.DrawToMM},
	} {
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) {
			errs = append(errs, fmt.Errorf("estimate: %s must be finite, got %v", f.name, f.v))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if !(c.MixC > c.ColdC) {
		errs = append(errs, fmt.Errorf("estimate: mix %v °C must be above cold %v °C", c.MixC, c.ColdC))
	}
	if c.MinC < c.MixC {
		errs = append(errs, fmt.Errorf("estimate: min %v °C must be at least mix %v °C (cooler water can't make mixed water)", c.MinC, c.MixC))
	}
	if c.DrawFromMM < 0 || !(c.DrawToMM > c.DrawFromMM) {
		errs = append(errs, fmt.Errorf("estimate: drawable band %v–%v mm must start at or above 0 and end above its start", c.DrawFromMM, c.DrawToMM))
	}
	if !(c.KWhPerLitreK > 0) {
		errs = append(errs, fmt.Errorf("estimate: kwh per litre per K must be above 0, got %v", c.KWhPerLitreK))
	}
	if !(c.LowFactor > 0 && c.LowFactor <= 1) {
		errs = append(errs, fmt.Errorf("estimate: low factor must be in (0, 1], got %v", c.LowFactor))
	}
	return errors.Join(errs...)
}

// Tank is the estimate for one tank.
type Tank struct {
	KWh          float64 // energy stored above ColdC (negative if the water is colder)
	HotL         float64 // litres at or above MinC
	UsableL40    float64 // litres of MixC water the drawable HotL make with ColdC water (plug flow)
	UsableL40Low float64 // UsableL40 × LowFactor
	MeanC        float64 // volume-weighted mean temperature (over the nominal capacity)
	TopC         float64 // the profile at the top of the water
}

// Estimator assesses profiles against one vessel and config.
type Estimator struct {
	v *vessel.Vessel
	c Config
}

// New validates c and returns an Estimator for v.
func New(v *vessel.Vessel, c Config) (*Estimator, error) {
	if v == nil {
		return nil, errors.New("estimate: nil vessel")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &Estimator{v: v, c: c}, nil
}

// Assess integrates the profile over the vessel's slices.
//
// The arithmetic follows avail.py term for term. The float64() conversions
// stop the compiler fusing multiply-adds (it may on arm64), so the sums
// match avail.py bit for bit given the same slices.
func (e *Estimator) Assess(p *profile.Profile) Tank {
	c := e.c
	var kwh, hotL, mix, meanNum float64
	e.v.Each(func(s vessel.Slice) {
		t := p.At(s.HeightMM)
		meanNum += float64(t * s.Litres)
		kwh += float64(float64(s.Litres*(t-c.ColdC)) * c.KWhPerLitreK)
		if t >= c.MinC {
			hotL += s.Litres
			if c.DrawFromMM <= s.HeightMM && s.HeightMM < c.DrawToMM {
				mix += float64(s.Litres*(t-c.ColdC)) / (c.MixC - c.ColdC)
			}
		}
	})
	return Tank{
		KWh:       kwh,
		HotL:      hotL,
		UsableL40: mix,
		// avail.py has no low estimate; it is mix40 × 0.85 in its report.
		UsableL40Low: mix * c.LowFactor,
		// avail.py divides by the nominal capacity, not the summed slices.
		MeanC: meanNum / e.v.Params().CapacityL,
		TopC:  p.At(e.v.Summary().TopMM),
	}
}
