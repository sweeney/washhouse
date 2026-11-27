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
// hottest water past it; the owner, 24 Nov 2026). How low scales with how
// far the water is above the cold feed: on 24 Nov the left read 8.5 °C low
// at 62 °C and the right 6 at 46, both 0.16–0.17 of the way down to the
// 10 °C feed. So the tank keeps that fraction, k: Config.PriorOutletUnderread
// until it has learned some (LearnOutletUnderread, from the engine: a rested
// draw's outlet peak and the reading just before it), then the median of the
// last OffsetWindow learned, with the prior counted as one until that many
// are learned, so one odd draw can't replace a good value (tanktemp's
// reviews of #12). For the estimate the outlet reads
// k/(1−k) × (its reading − the cold feed) higher, so a cold tank gets none:
// none while water flows (Flow), and after a draw it ramps in over SetRamp's
// time as the pipe cools. The sweep's probes stay raw, since the draw
// attribution reads their peaks.
//
// It is pure: no I/O and no clock. The time of a sweep is tanktemp's.
package tank

import (
	"errors"
	"fmt"
	"math"
	"slices"
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
	// PriorOutletUnderread: the fraction, in [0, 1), of the way from the
	// water's temperature down to the cold feed that the outlet probe reads
	// at rest, until one is learned (the package doc). 0 for none.
	PriorOutletUnderread float64
}

// OffsetWindow is how many learned under-reads the median is taken over.
const OffsetWindow = 5

// Validate reports the problems with c, or nil.
func (c Config) Validate() error {
	var errs []error
	if c.DeviceID == "" {
		errs = append(errs, errors.New("tank: device id is empty"))
	}
	if len(c.Probes) == 0 {
		errs = append(errs, errors.New("tank: no probes configured"))
	}
	if !(c.PriorOutletUnderread >= 0 && c.PriorOutletUnderread < 1) {
		errs = append(errs, fmt.Errorf("tank: prior outlet under-read %v must be in [0, 1)", c.PriorOutletUnderread))
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
	// RawEstimate is the estimate from the probes as read, without the
	// outlet's correction: the draw attribution compares energy across a
	// draw with it, since the correction is off while water flows and ramps
	// in after, which would read as heat lost or gained.
	RawEstimate estimate.Tank
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
	learned []float64     // the last OffsetWindow learned, oldest first
	ramp    time.Duration // how long after a draw it applies in full
	flowing bool          // a metered draw is running
	ended   time.Time     // when the last draw ended; zero before one
}

// LearnOutletUnderread adds a fraction learned from a draw; one outside
// (0, 1) is ignored.
func (t *Tank) LearnOutletUnderread(k float64) {
	if !(k > 0 && k < 1) {
		return
	}
	t.learned = append(t.learned, k)
	if len(t.learned) > OffsetWindow {
		t.learned = t.learned[len(t.learned)-OffsetWindow:]
	}
}

// OutletUnderreads is what has been learned, oldest first, to hand to a new
// tank (SetOutletUnderreads) when a reload rebuilds the engine.
func (t *Tank) OutletUnderreads() []float64 { return slices.Clone(t.learned) }

// SetOutletUnderreads replaces what has been learned with ks, oldest first.
func (t *Tank) SetOutletUnderreads(ks []float64) {
	t.learned = nil
	for _, k := range ks {
		t.LearnOutletUnderread(k)
	}
}

// FlowState is what Flow last told the tank, to hand to a new tank.
func (t *Tank) FlowState() (running bool, lastEnded time.Time) { return t.flowing, t.ended }

// SetRamp sets the time after a draw over which the offset ramps in.
func (t *Tank) SetRamp(d time.Duration) { t.ramp = d }

// underread is the median of the prior and what was learned, over the
// last OffsetWindow: the prior (if any) counts as one sample until
// OffsetWindow have been learned.
func (t *Tank) underread() float64 {
	var xs []float64
	if t.c.PriorOutletUnderread > 0 {
		xs = append(xs, t.c.PriorOutletUnderread)
	}
	xs = append(xs, t.learned...)
	if len(xs) > OffsetWindow {
		xs = xs[len(xs)-OffsetWindow:]
	}
	n := len(xs)
	if n == 0 {
		return 0
	}
	slices.Sort(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

// Flow tells the tank whether a metered draw is running, and when the last
// one ended.
func (t *Tank) Flow(running bool, lastEnded time.Time) {
	t.flowing, t.ended = running, lastEnded
}

// outletCorrection is what to add at at to an outlet reading readC:
// k/(1−k) × (readC − the cold feed), scaled by how far after the last draw
// at is, through the ramp.
func (t *Tank) outletCorrection(at time.Time, readC float64) float64 {
	k := t.underread()
	if k == 0 || t.flowing {
		return 0
	}
	full := k / (1 - k) * math.Max(readC-t.est.ColdC(), 0)
	if t.ended.IsZero() {
		return full // no draw seen: at rest
	}
	since := at.Sub(t.ended)
	if since <= 0 {
		return 0
	}
	if t.ramp <= 0 || since >= t.ramp {
		return full
	}
	return full * float64(since) / float64(t.ramp)
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
	raw, err := profile.New(slices.Clone(pp), profile.Options{TrustSuspect: t.c.TrustSuspect})
	if err != nil {
		return Sweep{}, fmt.Errorf("tank: %s: %w", st.DeviceID, err)
	}
	sw.RawEstimate = t.est.Assess(raw)
	// The profile gets the outlet corrected (sw.Probes keep the raw
	// reading); the bulk energy below leaves the outlet out anyway.
	for k := range pp {
		if pp[k].Role == profile.RoleOutlet && pp[k].TempC != nil {
			if corr := t.outletCorrection(st.Timestamp, *pp[k].TempC); corr > 0 {
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
