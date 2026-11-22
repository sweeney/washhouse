// Package profile turns a tank's probe readings into a temperature at any
// height: linear between probes, flat beyond the lowest and highest. By
// default it applies the stratification correction ('strat'): a running
// maximum from the bottom up, because a still tank cannot be colder above
// than below. The uncorrected 'raw' profile is a lower bound.
//
// It is a port of the profile in tanktemp's avail.py and gives the same
// numbers for the same readings; internal/estimate's parity test checks it.
//
// # Probe roles
//
// Each probe has a Role saying what it sits on (docs/HOT-WATER-SYSTEM.md
// §1). By default a role changes nothing, exactly as in avail.py:
//
//   - RoleOutlet (probe 5, the hot outlet) is heat-sunk by the copper pipe
//     and reads low at rest; strat already lifts it to the water below.
//   - RoleReturn (right probe 4, the secondary return) reads the return
//     water, below the tank; strat already lifts it too.
//   - RoleInlet (probe 1, the cold inlet) reads the bottom water and is the
//     first to see a draw; it is kept as it is.
//   - RoleBulk is an ordinary tank probe. The empty role means RoleBulk.
//
// A suspect reading (tanktemp's 85c_unverified: exactly 85 °C, which is
// also the DS18B20's power-on value) is unknown unless Options.TrustSuspect.
// WIRE.md calls it probably real, but no cylinder here gets near 85 °C, and
// strat would spread a false 85 up the whole tank (a suspect bottom 85 over
// 40, 60, 60, 60 doubles the stored energy). avail.py had no such rule.
//
// Options.Exclude is the opt-in extra: probes with an excluded role are
// dropped as if unknown (useful for the raw profile, which strat does not
// correct). A probe with no reading (TempC nil) is always skipped.
//
// TODO(phase 2): the top temperature should come from the outlet probe's
// last draw peak, decayed, rather than its resting value. The seam is the
// caller: the engine substitutes that value for the RoleOutlet probe's TempC
// before calling New, so no change is needed here.
package profile

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

// Role says what fitting a probe sits on.
type Role string

// The probe roles.
const (
	RoleBulk   Role = "bulk"
	RoleInlet  Role = "inlet"
	RoleOutlet Role = "outlet"
	RoleReturn Role = "return"
)

// Valid reports whether r is a known role (the empty role is bulk).
func (r Role) Valid() bool { return r.valid() }

func (r Role) valid() bool {
	switch r {
	case "", RoleBulk, RoleInlet, RoleOutlet, RoleReturn:
		return true
	}
	return false
}

// Probe is one probe's height, reading and role. TempC is nil when the
// reading is unknown.
type Probe struct {
	HeightMM float64
	TempC    *float64
	Role     Role
	Suspect  bool // tanktemp marked the reading suspect (85c_unverified)
}

// Options choose the profile variant. The zero value is avail.py's default:
// strat on, no role excluded.
type Options struct {
	Raw          bool   // skip the strat running max
	Exclude      []Role // drop probes with these roles (opt-in)
	TrustSuspect bool   // keep suspect readings (by default they are unknown)
}

// Point is one point of the effective profile, after skipping and strat.
type Point struct {
	HeightMM float64
	TempC    float64
}

// ErrTooFewProbes is returned when fewer than two probes are usable.
var ErrTooFewProbes = errors.New("profile: fewer than two usable probes")

// Profile is a temperature profile over height. It is immutable.
type Profile struct {
	pts []Point
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// New builds a profile from probes listed bottom first. Heights must be
// finite and strictly increasing (including those of unknown probes, since
// a bad height is a configuration fault), readings finite, and roles known.
func New(probes []Probe, o Options) (*Profile, error) {
	for _, r := range o.Exclude {
		if !r.valid() {
			return nil, fmt.Errorf("profile: unknown role %q in Exclude", r)
		}
	}
	pts := make([]Point, 0, len(probes))
	for i, p := range probes {
		if !finite(p.HeightMM) {
			return nil, fmt.Errorf("profile: probe %d height %v is not finite", i+1, p.HeightMM)
		}
		if i > 0 && !(p.HeightMM > probes[i-1].HeightMM) {
			return nil, fmt.Errorf("profile: probe %d at %v mm is not above probe %d at %v mm",
				i+1, p.HeightMM, i, probes[i-1].HeightMM)
		}
		if !p.Role.valid() {
			return nil, fmt.Errorf("profile: probe %d has unknown role %q", i+1, p.Role)
		}
		if p.TempC == nil || (p.Suspect && !o.TrustSuspect) {
			continue
		}
		if !finite(*p.TempC) {
			return nil, fmt.Errorf("profile: probe %d reading %v is not finite", i+1, *p.TempC)
		}
		if slices.Contains(o.Exclude, p.Role) {
			continue
		}
		pts = append(pts, Point{HeightMM: p.HeightMM, TempC: *p.TempC})
	}
	if len(pts) < 2 {
		return nil, ErrTooFewProbes
	}
	if !o.Raw {
		m := math.Inf(-1)
		for i := range pts {
			m = math.Max(m, pts[i].TempC)
			pts[i].TempC = m
		}
	}
	return &Profile{pts: pts}, nil
}

// At returns the temperature at height zMM.
func (p *Profile) At(zMM float64) float64 {
	pts := p.pts
	if zMM <= pts[0].HeightMM {
		return pts[0].TempC
	}
	last := pts[len(pts)-1]
	if zMM >= last.HeightMM {
		return last.TempC
	}
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		if a.HeightMM <= zMM && zMM <= b.HeightMM {
			return a.TempC + (b.TempC-a.TempC)*(zMM-a.HeightMM)/(b.HeightMM-a.HeightMM)
		}
	}
	panic("profile: unreachable") // heights are strictly increasing
}

// Points returns a copy of the effective profile points, bottom first.
func (p *Profile) Points() []Point {
	return slices.Clone(p.pts)
}
