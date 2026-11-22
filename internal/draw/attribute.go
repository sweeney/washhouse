package draw

import (
	"time"

	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/tank"
	"github.com/sweeney/washhouse/internal/wire/boiler"
)

// Reason says why a heat balance is unreliable.
type Reason string

// Reasons, in the order Attribute reports them.
const (
	// ReasonTankMissing: a tank had no usable before, share or balance
	// sweep, so the split (or, without a balance sweep, the total) is
	// unknown.
	ReasonTankMissing Reason = "tank_missing"
	// ReasonHWOn: the programmer's HW was on at some point in the window
	// (the cold inlet sits inside the coil zone, so a reheat warms the
	// water a draw brings in).
	ReasonHWOn Reason = "hw_on"
	// ReasonHWUnknown: the programmer's HW was unknown at some moment of
	// the window (including no HW input at all), so a reheat can't be ruled
	// out. It counts as masking: the shares are left unknown.
	ReasonHWUnknown Reason = "hw_unknown"
	// ReasonGained: a tank's stored energy rose across the draw.
	ReasonGained Reason = "tank_gained"
	// ReasonSmall: fewer metered litres than MinReliableLitres.
	ReasonSmall Reason = "small"
	// ReasonThermocline: a tank's cold layer sat between the gap probes at
	// the start.
	ReasonThermocline Reason = "thermocline_gap"
	// ReasonGapUnread: a gap probe had no reading at the start, so the
	// thermocline rule couldn't be checked.
	ReasonGapUnread Reason = "gap_unread"
	// ReasonInterrupted: the next draw began before the balance sweep, so
	// the balance was cut short at the last sweep before it.
	ReasonInterrupted Reason = "interrupted"
	// ReasonSharesEarly: some tank had no uninterrupted balance sweep, so
	// the shares come from the share sweep's losses, which read the split
	// before the probes have settled (on the 50 L shower, 0.76 right against
	// 0.89 settled).
	ReasonSharesEarly Reason = "shares_early"
	// ReasonNoLoss: the tanks lost no heat, so there is no split and no
	// volume.
	ReasonNoLoss Reason = "no_loss"
)

// Programmer is the boiler programmer's HW state from At on.
type Programmer struct {
	At time.Time
	HW boiler.Switch
}

// Window is what Attribute reads for one draw.
type Window struct {
	// Sweeps holds each tank's sweeps in time order. They must reach from
	// the last sweep before the first litre to the balance sweep; anything
	// outside that is ignored.
	Sweeps map[string][]tank.Sweep
	// HW is the programmer's HW state as a step function, in time order;
	// the first entry is the state before the window. Empty: unknown, so
	// only an observed gain can mask the draw.
	HW []Programmer
	// Cut, if set, is the next draw's first litre: no sweep at or after it
	// is used for the balance.
	Cut time.Time
}

// TankAttribution is one tank's part in a draw. Unknown is nil.
type TankAttribution struct {
	DeviceID string
	// Share is the tank's fraction of the metered litres, and Litres that
	// fraction of them. Unknown if any tank is missing, no tank lost heat,
	// or the draw is masked (a reheat, or HW unknown).
	Share  *float64
	Litres *float64
	// The stored energy lost from the before sweep (it can be negative:
	// the tank gained): EarlyLostKWh to the share sweep, SettledLostKWh to
	// the balance sweep, and LostKWh the one the shares used (the balance
	// sweep's, or the share sweep's with ReasonSharesEarly).
	LostKWh        *float64
	EarlyLostKWh   *float64
	SettledLostKWh *float64
	// OutletPeakC is the outlet probe's highest reading from the first
	// litre to the share sweep: the true top temperature (at rest the
	// outlet probe reads 3–8 °C low). Nil without an outlet reading; the
	// share then uses PeakUsedC from the before sweep's profile top
	// (OutletFromProfile).
	OutletPeakC       *float64
	PeakUsedC         *float64
	OutletFromProfile bool
	// InletDropC is the inlet probe's fall from the before sweep to its
	// lowest up to the share sweep.
	InletDropC *float64
	// The sweeps used; zero if none.
	Before, ShareAt, BalanceAt time.Time
}

// Attribution is one metered draw attributed to the tanks.
type Attribution struct {
	Draw  meter.Draw
	Tanks []TankAttribution // in Config.Tanks order
	// OutletC is the share-weighted outlet peak: the system's delivered
	// temperature.
	OutletC *float64
	// InletDropC is the largest inlet drop of any tank.
	InletDropC *float64
	// EnergyKWh is the heat the tanks lost by the balance sweep.
	EnergyKWh *float64
	// HeatBalanceL is EnergyKWh over what a litre at OutletC carries: the
	// probes' cross-check of the meter's litres (never the volume). With
	// every loss taken at one sweep it is the tanks' Litres summed. Zero,
	// never negative, when the tanks gained heat overall.
	HeatBalanceL *float64
	// Reliable is true when Reasons is empty.
	Reliable bool
	Reasons  []Reason
	// MaskedByReheat: ReasonHWOn or ReasonGained, a reheat seen. The
	// shares are also unknown with ReasonHWUnknown, a reheat not ruled out.
	MaskedByReheat bool
}

// Attribute attributes draw d (ended) over cfg's tanks from w (the package
// doc says which sweep gives what, and why). It is pure.
// cfg must validate.
//
// Each tank's sweeps are chosen so:
//   - before: the last sweep before the first litre, no older than
//     MaxSweepGap;
//   - share: the first sweep at or after the last litre + ShareSettle, no
//     later than MaxSweepGap past that;
//   - balance: the first sweep at or after the last litre + BalanceSettle,
//     likewise; or, if the next draw (w.Cut) starts first, the last sweep
//     before it (ReasonInterrupted), and never earlier than the share
//     sweep.
func Attribute(cfg Config, d meter.Draw, w Window) Attribution {
	a := Attribution{Draw: d}
	var reasons reasonSet
	shareTarget := d.End.Add(cfg.ShareSettle)
	balTarget := d.End.Add(cfg.BalanceSettle)
	missing := false
	sels := make([]selection, len(cfg.Tanks))
	for k, id := range cfg.Tanks {
		ss := w.Sweeps[id]
		ta := TankAttribution{DeviceID: id}
		var s selection
		s.before = lastBefore(ss, d.Start)
		if s.before != nil && d.Start.Sub(s.before.At) > cfg.MaxSweepGap {
			s.before = nil
		}
		s.share = firstFrom(ss, shareTarget, cfg.MaxSweepGap)
		s.bal = firstFrom(ss, balTarget, cfg.MaxSweepGap)
		if cut := w.Cut; !cut.IsZero() && s.share != nil {
			if !s.share.At.Before(cut) {
				// The next draw began before the share sweep: the split
				// and the balance both carry some of it.
				s.bal = s.share
				reasons.add(ReasonInterrupted)
			} else if (s.bal == nil || !s.bal.At.Before(cut)) && cut.Before(balTarget.Add(cfg.MaxSweepGap)) {
				// It began before the balance sweep: settle at the last
				// sweep before it.
				s.bal = lastBefore(ss, cut)
				reasons.add(ReasonInterrupted)
			}
		}
		if s.share == nil {
			s.bal = nil
		}
		if s.before == nil || s.share == nil {
			missing = true
		}
		if s.before != nil {
			ta.Before = s.before.At
		}
		if s.share != nil {
			ta.ShareAt = s.share.At
		}
		if s.bal != nil {
			ta.BalanceAt = s.bal.At
		}
		if s.before != nil {
			thermocline(cfg, s.before, &reasons)
		}
		if s.before != nil && s.share != nil {
			ta.EarlyLostKWh = ptr(s.before.Estimate.KWh - s.share.Estimate.KWh)
			ta.OutletPeakC = outletPeak(ss, d.Start, s.share.At)
			ta.InletDropC = inletDrop(ss, s.before, s.share.At)
			if ta.OutletPeakC != nil {
				ta.PeakUsedC = ta.OutletPeakC
			} else {
				ta.PeakUsedC = ptr(s.before.Estimate.TopC)
				ta.OutletFromProfile = true
			}
			if s.bal != nil {
				ta.SettledLostKWh = ptr(s.before.Estimate.KWh - s.bal.Estimate.KWh)
			}
		}
		if ta.InletDropC != nil && (a.InletDropC == nil || *ta.InletDropC > *a.InletDropC) {
			a.InletDropC = ptr(*ta.InletDropC)
		}
		sels[k] = s
		a.Tanks = append(a.Tanks, ta)
	}
	if missing {
		reasons.add(ReasonTankMissing)
	}

	// The shares' losses: to the balance sweep when every tank has one and
	// the next draw didn't cut it; else to the share sweep (shares_early).
	settled := !missing && !reasons[ReasonInterrupted]
	for _, ta := range a.Tanks {
		if ta.SettledLostKWh == nil {
			settled = false
		}
	}
	for k := range a.Tanks {
		if settled {
			a.Tanks[k].LostKWh = a.Tanks[k].SettledLostKWh
		} else {
			a.Tanks[k].LostKWh = a.Tanks[k].EarlyLostKWh
		}
	}
	if !missing && !settled {
		reasons.add(ReasonSharesEarly)
	}

	// Shares: each tank's litres are its heat lost over what a litre from
	// its own outlet carries; a tank that gained supplied none measurable.
	var shares []float64
	if !missing {
		litres := make([]float64, len(a.Tanks))
		sum := 0.0
		for k, ta := range a.Tanks {
			per := cfg.KWhPerLitreK * (*ta.PeakUsedC - cfg.ColdC)
			if per > 0 && *ta.LostKWh > 0 {
				litres[k] = *ta.LostKWh / per
			}
			sum += litres[k]
		}
		if sum > 0 {
			out := 0.0
			shares = make([]float64, len(a.Tanks))
			for k := range a.Tanks {
				shares[k] = litres[k] / sum
				out += shares[k] * *a.Tanks[k].PeakUsedC
			}
			a.OutletC = ptr(out)
		} else {
			reasons.add(ReasonNoLoss)
		}
	}

	// The heat balance: the energy used by the balance sweep over what a
	// litre at the system's outlet carries.
	total, haveAll := 0.0, !missing
	for _, ta := range a.Tanks {
		if ta.SettledLostKWh == nil {
			haveAll = false
			continue
		}
		total += *ta.SettledLostKWh
	}
	if !missing && !haveAll {
		reasons.add(ReasonTankMissing)
	}
	for k, ta := range a.Tanks {
		if g := gained(sels[k], ta); g != nil && *g > cfg.GainKWh {
			reasons.add(ReasonGained)
		}
	}
	if on, unknown := hwIn(w.HW, windowStart(sels), windowEnd(sels)); on || unknown {
		if on {
			reasons.add(ReasonHWOn)
		}
		if unknown {
			reasons.add(ReasonHWUnknown)
		}
	}
	if d.Litres < cfg.MinReliableLitres {
		reasons.add(ReasonSmall)
	}
	if haveAll {
		a.EnergyKWh = ptr(total)
		if total <= 0 {
			a.HeatBalanceL = ptr(0)
		} else if a.OutletC != nil && *a.OutletC > cfg.ColdC {
			a.HeatBalanceL = ptr(max(0, total/(cfg.KWhPerLitreK*(*a.OutletC-cfg.ColdC))))
		}
	}

	a.Reasons = reasons.list()
	a.Reliable = len(a.Reasons) == 0
	a.MaskedByReheat = reasons[ReasonHWOn] || reasons[ReasonGained]
	// A reheat (or HW unknown, which can't rule one out) heats whichever
	// tank its coil is in, so the split would measure the coil, not the
	// pipework: the shares are left unknown. The outlet and the balance
	// still use the computed split, and are unreliable.
	if shares != nil && !a.MaskedByReheat && !reasons[ReasonHWUnknown] {
		for k := range a.Tanks {
			a.Tanks[k].Share = ptr(shares[k])
			a.Tanks[k].Litres = ptr(shares[k] * d.Litres)
		}
	}
	return a
}

// thermocline flags a before sweep whose cold layer sits between a pair
// of gap probes: the low one below ThermoclineC, the high one at or above
// it.
func thermocline(cfg Config, s *tank.Sweep, r *reasonSet) {
	for _, g := range cfg.Gaps {
		lo, hi := readingAt(s, g.Low), readingAt(s, g.High)
		switch {
		case lo == nil || hi == nil:
			r.add(ReasonGapUnread)
		case *lo < cfg.ThermoclineC && *hi >= cfg.ThermoclineC:
			r.add(ReasonThermocline)
		}
	}
}

// readingAt is the usable reading of the probe at position, or nil.
func readingAt(s *tank.Sweep, position string) *float64 {
	for _, p := range s.Probes {
		if p.Position == position && p.TempC != nil && !p.Suspect {
			return p.TempC
		}
	}
	return nil
}

// hwIn says whether HW was on, and whether it was unknown, at any moment
// of [from, to]. Before the first state it is unknown. With no window (a
// tank missing its sweeps) neither is judged.
func hwIn(hw []Programmer, from, to time.Time) (on, unknown bool) {
	if from.IsZero() || to.IsZero() {
		return false, false
	}
	if len(hw) == 0 || hw[0].At.After(from) {
		unknown = true
	}
	for i, p := range hw {
		if p.At.After(to) {
			break
		}
		// p is in force somewhere in [from, to] unless the next change
		// comes at or before from.
		if i == len(hw)-1 || hw[i+1].At.After(from) {
			switch p.HW {
			case boiler.On:
				on = true
			case boiler.Unknown:
				unknown = true
			}
		}
	}
	return on, unknown
}

// gained is the heat a tank gained from its before sweep to its balance
// sweep. It compares the bulk probes' energy when both sweeps carry it: the
// full estimate "gains" when the heat-sunk outlet probe reads true during a
// tap. Without it, the full estimate's loss is used.
func gained(s selection, ta TankAttribution) *float64 {
	if s.before != nil && s.bal != nil && s.before.BulkKWh != nil && s.bal.BulkKWh != nil {
		return ptr(*s.bal.BulkKWh - *s.before.BulkKWh)
	}
	if ta.SettledLostKWh != nil {
		return ptr(-*ta.SettledLostKWh)
	}
	return nil
}

// windowStart is the earliest before sweep, windowEnd the latest balance
// sweep (or share sweep): the span a reheat would mask.
func windowStart(sels []selection) time.Time {
	var t time.Time
	for _, s := range sels {
		if s.before != nil && (t.IsZero() || s.before.At.Before(t)) {
			t = s.before.At
		}
	}
	return t
}

func windowEnd(sels []selection) time.Time {
	var t time.Time
	for _, s := range sels {
		for _, x := range []*tank.Sweep{s.share, s.bal} {
			if x != nil && x.At.After(t) {
				t = x.At
			}
		}
	}
	return t
}

// selection is one tank's chosen sweeps.
type selection struct {
	before, share, bal *tank.Sweep
}

// reasonSet collects reasons and lists them in declaration order.
type reasonSet map[Reason]bool

func (r *reasonSet) add(x Reason) {
	if *r == nil {
		*r = reasonSet{}
	}
	(*r)[x] = true
}

var reasonOrder = []Reason{ReasonTankMissing, ReasonHWOn, ReasonHWUnknown, ReasonGained, ReasonSmall, ReasonThermocline, ReasonGapUnread, ReasonInterrupted, ReasonSharesEarly, ReasonNoLoss}

func (r reasonSet) list() []Reason {
	var out []Reason
	for _, x := range reasonOrder {
		if r[x] {
			out = append(out, x)
		}
	}
	return out
}

func ptr(x float64) *float64 { return &x }

// lastBefore is the last sweep strictly before t, or nil.
func lastBefore(ss []tank.Sweep, t time.Time) *tank.Sweep {
	var out *tank.Sweep
	for i := range ss {
		if !ss[i].At.Before(t) {
			break
		}
		out = &ss[i]
	}
	return out
}

// firstFrom is the first sweep at or after t and no later than t + gap.
func firstFrom(ss []tank.Sweep, t time.Time, gap time.Duration) *tank.Sweep {
	for i := range ss {
		if ss[i].At.Before(t) {
			continue
		}
		if ss[i].At.Sub(t) > gap {
			return nil
		}
		return &ss[i]
	}
	return nil
}

// outletPeak is the highest outlet reading in sweeps from from to to.
func outletPeak(ss []tank.Sweep, from, to time.Time) *float64 {
	var peak *float64
	for i := range ss {
		if ss[i].At.Before(from) || ss[i].At.After(to) {
			continue
		}
		if o := ss[i].Outlet(); o != nil && (peak == nil || *o > *peak) {
			peak = ptr(*o)
		}
	}
	return peak
}

// inletDrop is the before sweep's inlet reading less the lowest after it,
// up to to; nil without an inlet reading at the before sweep or after it.
func inletDrop(ss []tank.Sweep, before *tank.Sweep, to time.Time) *float64 {
	b := before.Inlet()
	if b == nil {
		return nil
	}
	var low *float64
	for i := range ss {
		if !ss[i].At.After(before.At) || ss[i].At.After(to) {
			continue
		}
		if x := ss[i].Inlet(); x != nil && (low == nil || *x < *low) {
			low = ptr(*x)
		}
	}
	if low == nil {
		return nil
	}
	return ptr(*b - *low)
}
