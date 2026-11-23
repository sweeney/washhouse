package draw

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Config holds every value attribution uses. It has no defaults: the values
// come from washhouse's config, and Validate refuses a zero where one makes
// no sense.
type Config struct {
	// Tanks: the cylinders' device IDs, in the order attributions list
	// them. Every draw is attributed over all of them.
	Tanks []string
	// ColdC: the mains temperature, T_cold (10 °C). A litre from a tank's
	// outlet carries KWhPerLitreK × (outlet peak − ColdC).
	ColdC float64
	// KWhPerLitreK: water's heat capacity, c (4.186/3600 kWh per L·K).
	KWhPerLitreK float64
	// ShareSettle: the share sweep is each tank's first sweep at or after
	// the last litre + ShareSettle (60 s): long enough for the probes' 1–43
	// s lag. Outlet peaks and inlet drops come from it, the draw's end is
	// given once every tank has it (StageEnded), and the shares fall back
	// to it when the balance sweep is cut short (shares_early).
	ShareSettle time.Duration
	// BalanceSettle: the balance sweep is each tank's first sweep at or
	// after the last litre + BalanceSettle (8 min), at least ShareSettle.
	// The shares, the energy used and the heat-balance volume come from
	// it: the right tank's lower probe keeps falling for minutes after a
	// big draw, so the heat lost reads short (and the split wrong) until
	// then.
	BalanceSettle time.Duration
	// MaxSweepGap: a tank whose before sweep is older than this before the
	// first litre, or whose share or balance sweep comes more than this
	// after its target, is not reporting for that sweep (2 min: four
	// missed 30 s sweeps). It also bounds how long a pending draw waits.
	MaxSweepGap time.Duration
	// MinReliableLitres: a heat balance on a smaller draw is unreliable
	// (20 L: on 1–6 L draws it reads 3–5× too high, from the inlet probe's
	// transient dip).
	MinReliableLitres float64
	// GainKWh: a tank whose stored energy rose by more than this across the
	// draw (before sweep to balance sweep) gained heat, so a reheat masked
	// the draw (0.05 kWh, about a litre's worth; 0 means any rise).
	GainKWh float64
	// ThermoclineC and Gaps: at the before sweep, a tank whose Low probe
	// reads below ThermoclineC while its High probe reads at or above it
	// has its cold layer between the two, where the profile can't see it
	// move (T_min 40 °C; lower, 400 mm, to middle, 825 mm: a gap of about
	// 71 L in the 460 mm bore). Every pair in Gaps is checked.
	ThermoclineC float64
	Gaps         []Gap
	// OutletSplit: the fraction of every litre each tank gives through the
	// merged outlet (the pipework's split, in Tanks order; summing to 1).
	// When the shares are withheld (a reheat, or HW unknown, so the split
	// the probes give measures the coil), the delivered temperature and
	// the heat balance weight the outlet peaks by this instead. The engine
	// sets it from its own OutletSplit.
	OutletSplit []float64
}

// Gap names two probes by position (tanktemp's sensor positions, such as
// "lower" and "middle"), Low below High. Positions, not heights: a height
// re-measured by a millimetre must not silently stop the check. The engine
// checks that every tank has both positions.
type Gap struct {
	Low, High string
}

// Validate reports the problems with c, or nil.
func (c Config) Validate() error {
	var errs []error
	if len(c.Tanks) == 0 {
		errs = append(errs, errors.New("no Tanks set: at least one tank is needed"))
	}
	seen := map[string]bool{}
	for _, id := range c.Tanks {
		if id == "" {
			errs = append(errs, errors.New("a device id in Tanks is empty"))
		}
		if seen[id] {
			errs = append(errs, fmt.Errorf("%q listed twice in Tanks", id))
		}
		seen[id] = true
	}
	finite := func(name string, v float64) bool {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			errs = append(errs, fmt.Errorf("%s %v: must be finite", name, v))
			return false
		}
		return true
	}
	finite("ColdC", c.ColdC)
	if finite("KWhPerLitreK", c.KWhPerLitreK) && !(c.KWhPerLitreK > 0) {
		errs = append(errs, fmt.Errorf("KWhPerLitreK %v: must be > 0", c.KWhPerLitreK))
	}
	if c.ShareSettle <= 0 {
		errs = append(errs, fmt.Errorf("ShareSettle %v: must be > 0", c.ShareSettle))
	}
	if c.BalanceSettle < c.ShareSettle {
		errs = append(errs, fmt.Errorf("BalanceSettle %v: must be at least ShareSettle %v", c.BalanceSettle, c.ShareSettle))
	}
	if c.MaxSweepGap <= 0 {
		errs = append(errs, fmt.Errorf("MaxSweepGap %v: must be > 0", c.MaxSweepGap))
	}
	if finite("MinReliableLitres", c.MinReliableLitres) && !(c.MinReliableLitres > 0) {
		errs = append(errs, fmt.Errorf("MinReliableLitres %v: must be > 0", c.MinReliableLitres))
	}
	if finite("GainKWh", c.GainKWh) && c.GainKWh < 0 {
		errs = append(errs, fmt.Errorf("GainKWh %v: must be >= 0", c.GainKWh))
	}
	if finite("ThermoclineC", c.ThermoclineC) && !(c.ThermoclineC > c.ColdC) {
		errs = append(errs, fmt.Errorf("ThermoclineC %v: must be above ColdC %v", c.ThermoclineC, c.ColdC))
	}
	if len(c.Gaps) == 0 {
		errs = append(errs, errors.New("no Gaps set: at least one pair of gap probes is needed"))
	}
	seenGap := map[Gap]bool{}
	for _, g := range c.Gaps {
		if g.Low == "" || g.High == "" || g.Low == g.High {
			errs = append(errs, fmt.Errorf("the pair %q–%q in Gaps must name two different probe positions", g.Low, g.High))
		}
		if seenGap[g] {
			errs = append(errs, fmt.Errorf("the pair %q–%q listed twice in Gaps", g.Low, g.High))
		}
		seenGap[g] = true
	}
	if len(c.OutletSplit) != len(c.Tanks) {
		errs = append(errs, fmt.Errorf("OutletSplit has %d fractions for %d Tanks", len(c.OutletSplit), len(c.Tanks)))
	} else {
		sum := 0.0
		for _, x := range c.OutletSplit {
			if !(x >= 0 && x <= 1) {
				errs = append(errs, fmt.Errorf("OutletSplit %v: each fraction must be in [0, 1]", c.OutletSplit))
				break
			}
			sum += x
		}
		if math.Abs(sum-1) > 1e-9 {
			errs = append(errs, fmt.Errorf("OutletSplit %v sums to %v, not 1", c.OutletSplit, sum))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("draw config: %w", errors.Join(errs...))
	}
	return nil
}
