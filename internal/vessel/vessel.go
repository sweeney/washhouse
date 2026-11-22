// Package vessel models a hot-water cylinder's shape: a vertical cylinder
// with a shallow spherical-cap dome top and bottom, whose straight length is
// solved so the vessel holds its rated capacity. It slices the water into
// thin horizontal layers, each with a height and a volume, for the profile
// and estimate packages to integrate over.
//
// It is a port of the vessel model in tanktemp's avail.py and keeps that
// script's arithmetic (midpoint-rule slices, the same order of operations)
// so the two agree to rounding; internal/estimate's parity test checks it.
// Every number is a parameter: STI210 holds the defaults for the owner's
// Ariston Classico HE STI 210, most of which are guesses (see
// docs/HOT-WATER-SYSTEM.md §6).
package vessel

import (
	"errors"
	"fmt"
	"math"
)

// Params describes a vessel. Lengths are in millimetres, heights measured
// from the floor the cylinder stands on.
type Params struct {
	CapacityL       float64 // water the vessel holds, litres
	InnerDiameterMM float64 // inside diameter of the straight wall
	DomeDepthMM     float64 // depth of each spherical-cap dome (top and bottom)
	Z0MM            float64 // height of the inside of the bottom dome's apex
	SliceMM         float64 // thickness of each integration slice
}

// STI210 returns the parameters avail.py uses for the Ariston Classico HE
// STI 210 (550 mm outside, 1498 mm tall, 200 L). The inner diameter, dome
// depth and z0 are ASSUMED; avail.py slices at 1 mm.
func STI210() Params {
	return Params{
		CapacityL:       200,
		InnerDiameterMM: 460,
		DomeDepthMM:     60,
		Z0MM:            150,
		SliceMM:         1,
	}
}

func finitePositive(name string, x float64) error {
	if math.IsNaN(x) || math.IsInf(x, 0) || x <= 0 {
		return fmt.Errorf("vessel: %s must be a finite number above 0, got %v", name, x)
	}
	return nil
}

// Validate reports the first problem with p, or nil.
func (p Params) Validate() error {
	if err := errors.Join(
		finitePositive("capacity", p.CapacityL),
		finitePositive("inner diameter", p.InnerDiameterMM),
		finitePositive("dome depth", p.DomeDepthMM),
		finitePositive("slice", p.SliceMM),
	); err != nil {
		return err
	}
	if math.IsNaN(p.Z0MM) || math.IsInf(p.Z0MM, 0) || p.Z0MM < 0 {
		return fmt.Errorf("vessel: z0 must be a finite number at or above 0, got %v", p.Z0MM)
	}
	if p.DomeDepthMM > p.InnerDiameterMM/2 {
		return fmt.Errorf("vessel: dome depth %v mm is deeper than a hemisphere (radius %v mm)",
			p.DomeDepthMM, p.InnerDiameterMM/2)
	}
	// avail.py integrates each dome over int(depth) whole 1 mm slices; a
	// depth that isn't a whole number of slices would silently lose the
	// remainder, so it is refused rather than guessed at.
	if n := p.DomeDepthMM / p.SliceMM; n != math.Trunc(n) {
		return fmt.Errorf("vessel: dome depth %v mm must be a whole number of %v mm slices", p.DomeDepthMM, p.SliceMM)
	}
	if g := p.geometry(); g.straightMM <= 0 {
		return fmt.Errorf("vessel: capacity %v L is no more than the two domes hold (%v L)", p.CapacityL, 2*g.domeL)
	}
	return nil
}

// Slice is one horizontal layer of water: its centre height and the litres
// it holds.
type Slice struct {
	HeightMM float64
	Litres   float64
}

// Summary is the vessel model as avail.py --geometry prints it.
type Summary struct {
	DomeL       float64 // litres in each dome
	StraightMM  float64 // length of the straight wall, solved to hold CapacityL
	LitresPerCM float64 // litres per cm of straight wall
	BottomMM    float64 // bottom of the water (Z0MM)
	TopMM       float64 // top of the water
	TotalL      float64 // litres summed over the slices (CapacityL to rounding)
}

// Vessel is a validated, sliced vessel. It is immutable.
type Vessel struct {
	p      Params
	g      geometry
	slices []Slice
	totalL float64
}

type geometry struct {
	a, rSph, domeL, cylAreaMM2, straightMM, zCyl0, zCyl1, zTop float64
}

// capArea is the section area (mm²) at depth y into a dome, 0 at its apex.
// float64() stops the compiler fusing the multiply-add (it may on arm64),
// which keeps the result bit-for-bit with avail.py.
func capArea(rSph, y float64) float64 {
	return math.Pi * (float64(2*rSph*y) - float64(y*y))
}

func (p Params) geometry() geometry {
	var g geometry
	g.a = p.InnerDiameterMM / 2
	g.rSph = (g.a*g.a + p.DomeDepthMM*p.DomeDepthMM) / (2 * p.DomeDepthMM)
	n := int(p.DomeDepthMM / p.SliceMM)
	for i := 0; i < n; i++ {
		y := (float64(i) + 0.5) * p.SliceMM
		g.domeL += float64(capArea(g.rSph, y) * p.SliceMM)
	}
	g.domeL /= 1e6
	g.cylAreaMM2 = math.Pi * g.a * g.a
	g.straightMM = (p.CapacityL - 2*g.domeL) * 1e6 / g.cylAreaMM2
	g.zCyl0 = p.Z0MM + p.DomeDepthMM
	g.zCyl1 = g.zCyl0 + g.straightMM
	g.zTop = g.zCyl1 + p.DomeDepthMM
	return g
}

func (v *Vessel) area(z float64) float64 {
	g := v.g
	switch {
	case z < v.p.Z0MM || z > g.zTop:
		return 0
	case z < g.zCyl0:
		return capArea(g.rSph, z-v.p.Z0MM)
	case z <= g.zCyl1:
		return g.cylAreaMM2
	default:
		return capArea(g.rSph, g.zTop-z)
	}
}

// New validates p and slices the vessel. Slices run from the slice holding
// Z0MM to the slice holding the top of the water, each SliceMM thick, with
// the area taken at the slice's centre (as avail.py does at 1 mm).
func New(p Params) (*Vessel, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	v := &Vessel{p: p, g: p.geometry()}
	lo := int(math.Floor(p.Z0MM / p.SliceMM))
	hi := int(math.Floor(v.g.zTop / p.SliceMM))
	v.slices = make([]Slice, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		z := (float64(i) + 0.5) * p.SliceMM
		s := Slice{HeightMM: z, Litres: v.area(z) * p.SliceMM / 1e6}
		v.slices = append(v.slices, s)
		v.totalL += s.Litres
	}
	return v, nil
}

// Params returns the parameters the vessel was built from.
func (v *Vessel) Params() Params { return v.p }

// Summary returns the vessel model's headline figures.
func (v *Vessel) Summary() Summary {
	return Summary{
		DomeL:       v.g.domeL,
		StraightMM:  v.g.straightMM,
		LitresPerCM: v.g.cylAreaMM2 / 1e5,
		BottomMM:    v.p.Z0MM,
		TopMM:       v.g.zTop,
		TotalL:      v.totalL,
	}
}

// Slices returns a copy of the slices, bottom first.
func (v *Vessel) Slices() []Slice {
	return append([]Slice(nil), v.slices...)
}

// Each calls f for every slice, bottom first, without copying them.
func (v *Vessel) Each(f func(Slice)) {
	for _, s := range v.slices {
		f(s)
	}
}

// LitresBetween sums the slices whose centre is in [loMM, hiMM).
func (v *Vessel) LitresBetween(loMM, hiMM float64) float64 {
	var l float64
	for _, s := range v.slices {
		if loMM <= s.HeightMM && s.HeightMM < hiMM {
			l += s.Litres
		}
	}
	return l
}
