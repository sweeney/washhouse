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
// The hot-outlet probe is heat-sunk by its pipe: at rest it reads low, and
// only while water flows does it read the water's true temperature (the
// outlet's internal pipe reaches the top of the dome, so a draw brings the
// hottest water past it; the owner, 24 Nov 2026). For the estimate, the
// tank adds a learned offset to it (SetOutletOffset, from the engine: a
// draw's outlet peak less the probe's resting reading once settled): none
// while water flows (Flow), ramping in after a draw over the time the
// offset was measured across, and none before a draw has been seen. The
// sweep's probes stay raw, since the draw attribution reads their peaks.
//
// It is pure: no I/O and no clock. The time of a sweep is tanktemp's.
package tank

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/sweeney/common/wire/tanktemp"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/profile"
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
	// BulkKWh is the stored energy from the bulk and return probes only,
	// leaving out the inlet and outlet: they sit on fittings that move with
	// a sub-litre draw, and the heat-sunk outlet reads low at rest but true
	// during flow, which the full estimate reads as heat gained. Use it to
	// compare energy across a draw. nil with fewer than two such probes.
	BulkKWh *float64
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

	// The outlet probe's heat-sink correction (see the package doc).
	offsetC float64       // learned; 0 for none
	ramp    time.Duration // how long after a draw it applies in full
	flowing bool          // a metered draw is running
	ended   time.Time     // when the last draw ended; zero before one
}

// SetOutletOffset sets the outlet probe's heat-sink offset, in °C, and the
// time after a draw over which it ramps in. An offset at or below 0 is none.
func (t *Tank) SetOutletOffset(offsetC float64, ramp time.Duration) {
	t.offsetC, t.ramp = math.Max(offsetC, 0), ramp
}

// Flow tells the tank whether a metered draw is running, and when the last
// one ended.
func (t *Tank) Flow(running bool, lastEnded time.Time) {
	t.flowing, t.ended = running, lastEnded
}

// outletCorrection is what to add to the outlet probe at at: the offset,
// scaled by how far after the last draw at is, through the ramp.
func (t *Tank) outletCorrection(at time.Time) float64 {
	if t.offsetC == 0 || t.flowing || t.ended.IsZero() {
		return 0
	}
	since := at.Sub(t.ended)
	if since <= 0 {
		return 0
	}
	if t.ramp <= 0 || since >= t.ramp {
		return t.offsetC
	}
	return t.offsetC * float64(since) / float64(t.ramp)
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
	// The profiles get the outlet corrected (sw.Probes keep the raw
	// reading); the bulk energy below leaves the outlet out anyway.
	if corr := t.outletCorrection(st.Timestamp); corr > 0 {
		for k := range pp {
			if pp[k].Role == profile.RoleOutlet && pp[k].TempC != nil {
				pp[k].TempC = ptr(*pp[k].TempC + corr)
			}
		}
	}
	prof, err := profile.New(pp, profile.Options{TrustSuspect: t.c.TrustSuspect})
	if err != nil {
		return Sweep{}, fmt.Errorf("tank: %s: %w", st.DeviceID, err)
	}
	sw.Profile = prof
	sw.Estimate = t.est.Assess(prof)
	bulk, err := profile.New(pp, profile.Options{TrustSuspect: t.c.TrustSuspect,
		Exclude: []profile.Role{profile.RoleInlet, profile.RoleOutlet}})
	if err == nil {
		k := t.est.Assess(bulk).KWh
		sw.BulkKWh = &k
	} else if !errors.Is(err, profile.ErrTooFewProbes) {
		return Sweep{}, fmt.Errorf("tank: %s: %w", st.DeviceID, err)
	}
	return sw, nil
}

func ptr(x float64) *float64 { return &x }
