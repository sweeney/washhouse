// Package tank turns one tanktemp sweep of a cylinder into what the rest of
// washhouse reasons with: its probes placed and given roles, the
// temperature profile, and the estimate (stored energy, hot and usable
// litres).
//
// Roles come from config, by position: they say what fitting each probe
// sits on (docs/HOT-WATER-SYSTEM.md §1), e.g. the right tank's upper probe
// is on the secondary return. Heights come from tanktemp's payload, with the
// config's as the fallback. A failed read is unknown; so is a suspect one
// (85c_unverified) unless the config trusts suspects (package profile).
//
// It is pure: no I/O and no clock. The time of a sweep is tanktemp's.
package tank

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/wire/tanktemp"
)

// ProbeConfig is one probe position's role and fallback height.
type ProbeConfig struct {
	Position string
	Role     profile.Role
	HeightMM float64 // used when tanktemp's payload has none; 0: no fallback
}

// Config describes one cylinder's probes.
type Config struct {
	DeviceID     string
	Probes       []ProbeConfig
	TrustSuspect bool
}

// Validate reports the problems with c, or nil.
func (c Config) Validate() error {
	var errs []error
	if c.DeviceID == "" {
		errs = append(errs, errors.New("tank: device id is empty"))
	}
	if len(c.Probes) == 0 {
		errs = append(errs, errors.New("tank: no probes configured"))
	}
	seen := map[string]bool{}
	for _, p := range c.Probes {
		if p.Position == "" {
			errs = append(errs, errors.New("tank: a probe has no position"))
		}
		if seen[p.Position] {
			errs = append(errs, fmt.Errorf("tank: position %q configured twice", p.Position))
		}
		seen[p.Position] = true
		if !p.Role.Valid() {
			errs = append(errs, fmt.Errorf("tank: position %q has unknown role %q", p.Position, p.Role))
		}
		if math.IsNaN(p.HeightMM) || math.IsInf(p.HeightMM, 0) || p.HeightMM < 0 {
			errs = append(errs, fmt.Errorf("tank: position %q height %v must be finite and at or above 0", p.Position, p.HeightMM))
		}
	}
	return errors.Join(errs...)
}

// Probe is one probe as placed in a sweep.
type Probe struct {
	Position string
	Role     profile.Role
	HeightMM float64
	TempC    *float64 // tanktemp's reading; nil if the read failed
	Suspect  bool     // tanktemp marked it suspect
}

// Sweep is one cylinder at one instant.
type Sweep struct {
	DeviceID    string
	At          time.Time // tanktemp's timestamp
	ClockSynced bool
	Probes      []Probe // bottom first
	ProbesOK    int     // probes with a reading the profile used
	ProbesTotal int
	Profile     *profile.Profile
	Estimate    estimate.Tank
}

// Inlet is the cold-inlet probe's reading, or nil.
func (s Sweep) Inlet() *float64 { return s.byRole(profile.RoleInlet) }

// Outlet is the hot-outlet probe's reading, or nil.
func (s Sweep) Outlet() *float64 { return s.byRole(profile.RoleOutlet) }

func (s Sweep) byRole(r profile.Role) *float64 {
	for _, p := range s.Probes {
		if p.Role == r && p.TempC != nil && !p.Suspect {
			return p.TempC
		}
	}
	return nil
}

// Tank builds sweeps for one cylinder.
type Tank struct {
	c   Config
	by  map[string]ProbeConfig
	est *estimate.Estimator
}

// New validates c and returns a Tank estimating with est.
func New(c Config, est *estimate.Estimator) (*Tank, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if est == nil {
		return nil, errors.New("tank: nil estimator")
	}
	by := make(map[string]ProbeConfig, len(c.Probes))
	for _, p := range c.Probes {
		by[p.Position] = p
	}
	return &Tank{c: c, by: by, est: est}, nil
}

// Sweep places st's probes and estimates the tank. It refuses a state from
// another cylinder, an unconfigured position, a probe it can't place, and a
// state with fewer than two usable probes.
func (t *Tank) Sweep(st tanktemp.State) (Sweep, error) {
	if st.DeviceID != t.c.DeviceID {
		return Sweep{}, fmt.Errorf("tank: state from %q, not %q", st.DeviceID, t.c.DeviceID)
	}
	sw := Sweep{DeviceID: st.DeviceID, At: st.Timestamp, ClockSynced: st.ClockSynced, ProbesTotal: len(st.Sensors)}
	pp := make([]profile.Probe, 0, len(st.Sensors))
	for _, sn := range st.Sensors {
		pc, ok := t.by[sn.Position]
		if !ok {
			return Sweep{}, fmt.Errorf("tank: %s: position %q isn't configured", st.DeviceID, sn.Position)
		}
		h := pc.HeightMM
		if sn.HeightMM != nil {
			h = float64(*sn.HeightMM)
		}
		if h <= 0 {
			return Sweep{}, fmt.Errorf("tank: %s: position %q has no height", st.DeviceID, sn.Position)
		}
		p := Probe{Position: sn.Position, Role: pc.Role, HeightMM: h, TempC: sn.TempC, Suspect: sn.Suspect != nil}
		sw.Probes = append(sw.Probes, p)
		if p.TempC != nil && (!p.Suspect || t.c.TrustSuspect) {
			sw.ProbesOK++
		}
		pp = append(pp, profile.Probe{HeightMM: h, TempC: p.TempC, Role: p.Role, Suspect: p.Suspect})
	}
	prof, err := profile.New(pp, profile.Options{TrustSuspect: t.c.TrustSuspect})
	if err != nil {
		return Sweep{}, fmt.Errorf("tank: %s: %w", st.DeviceID, err)
	}
	sw.Profile = prof
	sw.Estimate = t.est.Assess(prof)
	return sw, nil
}
