// Package thermal follows each cylinder's heat: its power (dE/dt), its state
// (heating, drawing, cooling or unknown), the source of any heating (the
// programmer's HW or CH channel), and an online fit of its standing loss in
// W/K against the boiler room.
//
// It is pure: no I/O, no goroutines, no wall clock. Time is event time, from
// the inputs: tanktemp's sweep timestamps, the programmer's switch times, the
// boiler-room climate readings and the meter's draw times (and Tick, which
// the caller drives). Feed inputs in arrival order; each call returns the
// events it decided. Every threshold is in Config; none is in code.
//
// The rules, and where they come from:
//   - power: the least-squares slope of the tank's energy over the last
//     PowerWindow of sweeps (they come every 10–30 s, quantised to 1/16 °C).
//     The energy is the estimate's, but from the probes whose roles are not
//     in PowerExclude: the inlet (probe 1) and outlet (probe 5) sit on
//     fittings and answer to flow, sub-litre draws the meter can't count
//     included, more than to heat in the water (HOT-WATER-SYSTEM.md §1).
//     With them in, a CH-only spell that heats nothing (ch-only-no-heat)
//     shows +1.5–2 kW on the right tank after small draws; without them it
//     shows none, and reheats look the same. Over a whole reheat or night
//     the two energies change alike. When the set of usable probes changes
//     the energy steps for no physical reason, so the window starts again.
//     After a draw, power is unknown until PowerSettle has passed (the
//     inlet plume settling and the probes' lag, 12 min from the 50 L
//     shower); then the window starts again from there, so power is known
//     again PowerMinSpan later (17–18 min after the draw's end).
//   - state: unknown while the tank's input is stale (no sweep for
//     StaleAfter) or too thin (fewer than MinProbes usable probes), or
//     before its power has first been judged; else drawing while a metered
//     draw runs and for DrawSettle after it (the probes lag the meter by up
//     to 43 s); else heating or cooling, from the power with hysteresis
//     (heating from HeatingOnKW, until it falls below HeatingOffKW).
//   - a draw is the system's: both tanks supply it through the merged
//     outlet, so both are drawing.
//   - heating is judged only from a power window clear of draws (no draw
//     running or settling at any point in it). Until then the last judgement
//     holds: a reheat interrupted by a bath is heating again after it with
//     no heating_ended between, and a tank that was cooling stays cooling.
//     After a 4 L tap the right tank's bulk energy recovers at up to
//     0.8 kW (its return-fitting probe re-warming) with no heat put in, so a
//     window holding a draw can't say heating. The price: during a reheat
//     with draws every few minutes (bath-in-reheat) the window never clears,
//     and a tank whose heating wasn't yet judged is unknown between draws.
//   - source: only a tank judged heating has one, so no source is ever
//     assumed from the programmer alone (the right tank heated on CH-only
//     calls until its valve was replaced on 19 Nov, and since then CH-only
//     spells heat neither tank). HW on is hw; HW off and CH on is ch; with
//     both off, the channel that switched off within SourceHold (HW first),
//     since the coil's heat keeps reaching the probes for a while (17
//     minutes after CH_OFF on 8 Nov); else null. HW unknown is never ch.
//     A channel going unknown is a break, not a switch-off: it ends any
//     hold, so a dropped boiler sensor doesn't keep the source for
//     SourceHold.
//   - loss fit: UA = loss / (T_mean − T_room), loss = −dE/dt, both from the
//     power's energy, sampled at each sweep while the tank has been cooling
//     for UAQuiet, its power window is clear of draws, the room reading is
//     no older than AmbientStale, and the tank is at least UAMinDeltaK above
//     the room. Each sample is weighted by the hours since the previous one;
//     the fit is the weighted mean, seeded with the configured prior worth
//     PriorHours, and every weight decays by exp(−Δt/UATau) in event time.
//     The prior and old evidence decay alike, so time alone never moves the
//     fit; it only makes room for new evidence. With UATau a day, a night
//     after a step carries about 3/4 of the fit and two nights about 9/10.
//     No room reading, no fit.
//   - the loss fit's room is the boiler room, as specified. The right
//     tank's return loop sheds its heat to the house's pipework instead:
//     with the loop running its loss fits 18 W/K × (T − 14 °C), so against
//     the boiler room (23–28 °C overnight) it reads 33–39 W/K, about twice
//     PLAN.md's 15–19 W/K. That is the loop's signature, not a fault.
//     The offset's direction says why: a pumpless loop's flow rises with
//     temperature, so its loss is superlinear in T, and a line through
//     superlinear points would put the reference above the room. It comes
//     out about 10 °C below, which points to a cold sink (pipework through
//     colder parts of the house), not to the thermosiphon's curve. Two
//     cautions: the boiler room isn't an independent ambient (the tanks and
//     boiler heat it, so a fit against it partly regresses the tank on
//     itself), and over one night T and the house fall together, so the
//     14 °C intercept is an extrapolation across a narrow range, weakly
//     pinned.
//   - the right tank's P4 (upper, role return) sits on the return fitting
//     and reads the return water while the loop runs: T4 − mean(T3,T5) is
//     about −5.6 K then, against about +2.5 K with the loop stopped. Kept in
//     the energy, it lowers meanC by a few K while the loop runs, inflating
//     the loop-running UA by roughly 10% (power, a slope, is barely moved),
//     and steps the energy when the loop changes state. With the return
//     valve closed the right tank should read about 6 W/K against the
//     boiler room (as on the airlocked nights), with the left (3.6–4.7 W/K)
//     as the control; the difference, about 2 W/K, is the stopped loop's
//     stubs and fitting.
//   - sweeps with an unsynced clock, or no later than the tank's last, are
//     skipped. A metered draw is known only once the meter consumer decides
//     it (seconds after its first litre); sweeps before that were judged
//     without it.
package thermal

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/sweeney/common/wire/boiler"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/tank"
)

// TankConfig is one cylinder and the prior for its loss fit.
type TankConfig struct {
	DeviceID     string
	PriorUAWPerK float64 // the loss fit's starting value, W/K
	PriorHours   float64 // how many hours of evidence the prior is worth
}

// Config holds every threshold and time constant. It has no defaults: the
// values come from washhouse's config, and Validate refuses a zero.
type Config struct {
	// PowerWindow: power is the least-squares slope over this much of the
	// latest sweeps (10 min). PowerMinSpan and PowerMinSamples: with fewer
	// samples, or spanning less, the power is unknown (5 min, 3).
	PowerWindow     time.Duration
	PowerMinSpan    time.Duration
	PowerMinSamples int
	// PowerExclude: probe roles left out of the energy that power and the
	// loss fit use (inlet and outlet). Empty: the sweep's own estimate.
	PowerExclude []profile.Role
	// MinProbes: fewer usable probes (after PowerExclude) is unknown (2).
	MinProbes int
	// StaleAfter: no sweep for longer than this is unknown (2 min).
	StaleAfter time.Duration
	// HeatingOnKW and HeatingOffKW: heating starts at a power of at least
	// on, and ends below off (0.5, 0.3 kW).
	HeatingOnKW  float64
	HeatingOffKW float64
	// HeatingOnAfterDrawKW: inside PowerSettle, while the power is still
	// unknown, a tank not yet heating is judged heating if HW is on now
	// and its energy, from the sweeps after
	// DrawSettle, rises at least this fast over PowerMinSpan. A draw's
	// settling looked like up to 0.72 kW with no heat going in (CH on in
	// ch-only-no-heat); reheats show 2–9 kW. 1.5 kW calls a reheat about 6
	// min after a draw rather than 17 (the owner, 24 Nov 2026). Cooling
	// still waits for the whole window. At least HeatingOnKW.
	HeatingOnAfterDrawKW float64
	// DrawSettle: a tank is drawing for this long after a metered draw
	// ends (60 s).
	DrawSettle time.Duration
	// PowerSettle: after a metered draw ends, the bulk probes keep falling
	// for a while. No more cold water arrives once the flow stops; what
	// they see is the inlet plume settling, and their own lag. That is not
	// heat lost, so power is unknown, and heating isn't judged, until the
	// power window starts this long after the draw's end (12 min, an
	// empirical figure); at least DrawSettle. Once a sweep is past it, the
	// samples from inside it are dropped, so power returns after about
	// PowerSettle + PowerMinSpan.
	PowerSettle time.Duration
	// SourceHold: with both channels off, heating is still the source of
	// the channel that switched off up to this long ago (30 min).
	SourceHold time.Duration
	// UATau: the loss fit's forgetting time constant (24 h).
	UATau time.Duration
	// UAQuiet: the loss fit takes evidence only after the tank has been
	// cooling this long (60 min); at least PowerWindow.
	UAQuiet time.Duration
	// UAMinDeltaK: no evidence from a tank less than this above the room
	// (5 K).
	UAMinDeltaK float64
	// AmbientStale: a room reading older than this is no use (40 min).
	AmbientStale time.Duration
	Tanks        []TankConfig
}

// Validate reports the problems with c, or nil.
func (c Config) Validate() error {
	var errs []error
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"power window", c.PowerWindow}, {"power min span", c.PowerMinSpan},
		{"stale after", c.StaleAfter}, {"draw settle", c.DrawSettle}, {"power settle", c.PowerSettle},
		{"source hold", c.SourceHold}, {"UA tau", c.UATau}, {"UA quiet", c.UAQuiet},
		{"ambient stale", c.AmbientStale},
	} {
		if d.v <= 0 {
			errs = append(errs, fmt.Errorf("thermal: %s must be above 0, got %v", d.name, d.v))
		}
	}
	if c.PowerSettle < c.DrawSettle {
		errs = append(errs, fmt.Errorf("thermal: power settle %v must be at least the draw settle %v", c.PowerSettle, c.DrawSettle))
	}
	if c.PowerMinSpan > c.PowerWindow {
		errs = append(errs, fmt.Errorf("thermal: power min span %v must be within the power window %v", c.PowerMinSpan, c.PowerWindow))
	}
	if c.UAQuiet < c.PowerWindow {
		errs = append(errs, fmt.Errorf("thermal: UA quiet %v must be at least the power window %v, so a UA sample's window is all quiet", c.UAQuiet, c.PowerWindow))
	}
	if c.PowerMinSamples < 3 {
		errs = append(errs, fmt.Errorf("thermal: power min samples must be at least 3, got %d", c.PowerMinSamples))
	}
	if c.MinProbes < 2 {
		errs = append(errs, fmt.Errorf("thermal: min probes must be at least 2 (a profile needs two), got %d", c.MinProbes))
	}
	for _, r := range c.PowerExclude {
		if !r.Valid() {
			errs = append(errs, fmt.Errorf("thermal: power exclude has unknown role %q", r))
		}
	}
	if !finite(c.HeatingOnKW) || c.HeatingOnKW <= 0 {
		errs = append(errs, fmt.Errorf("thermal: heating on must be finite and above 0 kW, got %v", c.HeatingOnKW))
	}
	if !finite(c.HeatingOnAfterDrawKW) || c.HeatingOnAfterDrawKW < c.HeatingOnKW {
		errs = append(errs, fmt.Errorf("thermal: heating on after a draw must be finite and at least heating on (%v kW), got %v", c.HeatingOnKW, c.HeatingOnAfterDrawKW))
	}
	if !finite(c.HeatingOffKW) || c.HeatingOffKW > c.HeatingOnKW {
		errs = append(errs, fmt.Errorf("thermal: heating off must be finite and at most heating on (%v kW), got %v", c.HeatingOnKW, c.HeatingOffKW))
	}
	if !finite(c.UAMinDeltaK) || c.UAMinDeltaK <= 0 {
		errs = append(errs, fmt.Errorf("thermal: UA min delta must be finite and above 0 K, got %v", c.UAMinDeltaK))
	}
	if len(c.Tanks) == 0 {
		errs = append(errs, errors.New("thermal: no tanks configured"))
	}
	seen := map[string]bool{}
	for _, tc := range c.Tanks {
		if tc.DeviceID == "" {
			errs = append(errs, errors.New("thermal: a tank has no device id"))
		}
		if seen[tc.DeviceID] {
			errs = append(errs, fmt.Errorf("thermal: tank %q configured twice", tc.DeviceID))
		}
		seen[tc.DeviceID] = true
		if !finite(tc.PriorUAWPerK) || tc.PriorUAWPerK <= 0 {
			errs = append(errs, fmt.Errorf("thermal: tank %q prior UA must be finite and above 0 W/K, got %v", tc.DeviceID, tc.PriorUAWPerK))
		}
		if !finite(tc.PriorHours) || tc.PriorHours <= 0 {
			errs = append(errs, fmt.Errorf("thermal: tank %q prior hours must be finite and above 0, got %v", tc.DeviceID, tc.PriorHours))
		}
	}
	return errors.Join(errs...)
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// State is a tank's thermal state.
type State string

// The states.
const (
	StateUnknown State = "unknown"
	StateHeating State = "heating"
	StateDrawing State = "drawing"
	StateCooling State = "cooling"
)

// Source is what is heating a tank. SourceNone is null on the wire.
type Source string

// The sources.
const (
	SourceNone Source = ""
	SourceHW   Source = "hw"
	SourceCH   Source = "ch"
)

// EventType says what an Event reports.
type EventType string

// The event types.
const (
	EventStateChanged   EventType = "tank_state_changed"
	EventHeatingStarted EventType = "heating_started"
	EventHeatingEnded   EventType = "heating_ended"
)

// Event is one change the tracker decided, at its event time.
// tank_state_changed is sent when the state or the source changes;
// heating_started and heating_ended when the heating judgement does
// (heating held through a draw is not ended by it). Source is the new one,
// or, for heating_ended, the episode's last. PowerKW is nil when unknown.
type Event struct {
	Type     EventType
	DeviceID string
	At       time.Time
	State    State // the state after the change
	Prev     State // the state before it
	Source   Source
	PowerKW  *float64
}

// UA is a tank's loss fit.
type UA struct {
	WPerK         float64 // the fit, W/K against the boiler room
	EvidenceHours float64 // hours of evidence taken, in all (not decayed)
	PriorShare    float64 // the prior's share of the fit's weight, 0–1
}

// Status is a tank's thermal state as of its latest input.
type Status struct {
	DeviceID string
	At       time.Time // the latest sweep used; zero if none
	State    State
	Source   Source   // SourceNone unless State is heating
	PowerKW  *float64 // nil when unknown
	UA       UA
}

// Tracker follows every configured tank. It is not safe for concurrent
// use: one goroutine feeds it.
type Tracker struct {
	cfg   Config
	est   *estimate.Estimator
	tanks map[string]*tankTrack
	order []string

	hw, ch channel // the programmer

	drawRunning bool      // a metered draw is running
	drawEnded   bool      // a metered draw has ended
	drawEnd     time.Time // when the last one ended

	ambOK bool // a room temperature has been read
	ambAt time.Time
	ambC  float64
}

// drawing says whether a metered draw runs at at, or ended no more than
// DrawSettle before it (the probes lag the meter).
func (t *Tracker) drawing(at time.Time) bool {
	return t.drawRunning || (t.drawEnded && at.Sub(t.drawEnd) <= t.cfg.DrawSettle)
}

// clear says whether a power window starting at from holds no drawing,
// nor the probes' catch-up after one (PowerSettle).
func (t *Tracker) clear(from time.Time) bool {
	return !t.drawRunning && (!t.drawEnded || from.Sub(t.drawEnd) > t.cfg.PowerSettle)
}

// channel is one programmer channel: its state, and when it last switched
// off from on.
type channel struct {
	sw    boiler.Switch
	wasOn bool
	offAt time.Time
}

// set takes the channel's state as of at. Only on → off is a switch-off;
// unknown is a break (the boiler sensor dropped, so it may have switched
// on, or stayed on, unseen), and ends any hold.
func (c *channel) set(at time.Time, sw boiler.Switch) {
	switch {
	case sw == boiler.Unknown:
		c.wasOn = false
	case c.sw == boiler.On && sw == boiler.Off:
		c.wasOn, c.offAt = true, at
	}
	c.sw = sw
}

// held says whether c switched off from on no more than hold before at.
func (c channel) held(at time.Time, hold time.Duration) bool {
	return c.sw != boiler.On && c.wasOn && at.Sub(c.offAt) <= hold
}

// sample is one sweep's energy, as power and the loss fit use it.
type sample struct {
	at    time.Time
	kwh   float64
	meanC float64
}

type tankTrack struct {
	cfg       TankConfig
	seen      bool
	at        time.Time // the latest sweep used
	probesOK  bool      // it had enough usable probes
	sig       uint64    // which probes the energy used
	samples   []sample  // within PowerWindow of at, all with sig
	power     *float64
	powerFrom time.Time // the first sample the power used

	decided  bool // heating is known (power has been judged since the tank was last unknown)
	heating  bool
	state    State
	source   Source
	lastEval time.Time // when state was last settled
	reported bool      // heating, as the last events said
	episode  Source    // the heating episode's latest source

	// The loss fit: a forgetting weighted mean of loss/ΔT samples, each
	// weighted by its hours, seeded with the prior's hours.
	quietSince time.Time // cooling since; zero when not cooling
	lastFit    time.Time // the last sample's time; zero after a break
	uaSum      float64   // Σ w·UA
	wSum       float64   // Σ w, hours, decayed
	priorW     float64   // the prior's part of wSum
	uaAt       time.Time // when the sums were last decayed
	evidenceH  float64   // hours of evidence, not decayed
}

// New validates cfg and returns a tracker. est is the estimator for the
// energy power is computed from; it may be nil only when PowerExclude is
// empty (power is then the sweep's own estimate).
func New(cfg Config, est *estimate.Estimator) (*Tracker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if len(cfg.PowerExclude) > 0 && est == nil {
		return nil, errors.New("thermal: power exclude needs an estimator")
	}
	t := &Tracker{cfg: cfg, est: est, tanks: map[string]*tankTrack{}}
	for _, tc := range cfg.Tanks {
		t.tanks[tc.DeviceID] = &tankTrack{cfg: tc, state: StateUnknown,
			uaSum: tc.PriorUAWPerK * tc.PriorHours, wSum: tc.PriorHours, priorW: tc.PriorHours}
		t.order = append(t.order, tc.DeviceID)
	}
	return t, nil
}

// energy is the sweep's energy and mean temperature from the probes power
// uses, with a signature of which probes those were, or ok false if fewer
// than MinProbes were usable.
func (t *Tracker) energy(sw tank.Sweep) (kwh, meanC float64, sig uint64, ok bool) {
	pp := make([]profile.Probe, 0, len(sw.Probes))
	n := 0
	for i, p := range sw.Probes {
		usable := p.TempC != nil && !p.Suspect && !slices.Contains(t.cfg.PowerExclude, p.Role)
		if usable {
			sig |= 1 << uint(i)
			n++
		}
		pp = append(pp, profile.Probe{HeightMM: p.HeightMM, TempC: p.TempC, Role: p.Role, Suspect: p.Suspect})
	}
	if n < t.cfg.MinProbes {
		return 0, 0, 0, false
	}
	if len(t.cfg.PowerExclude) == 0 {
		return sw.Estimate.KWh, sw.Estimate.MeanC, sig, true
	}
	prof, err := profile.New(pp, profile.Options{Exclude: t.cfg.PowerExclude})
	if err != nil {
		return 0, 0, 0, false
	}
	e := t.est.Assess(prof)
	return e.KWh, e.MeanC, sig, true
}

// Sweep takes a tank's sweep. A sweep with an unsynced clock, or no later
// than the tank's last, is skipped; one from a tank not configured is an
// error.
func (t *Tracker) Sweep(sw tank.Sweep) ([]Event, error) {
	tk := t.tanks[sw.DeviceID]
	if tk == nil {
		return nil, fmt.Errorf("thermal: sweep from %q, which isn't configured", sw.DeviceID)
	}
	if !sw.ClockSynced || (tk.seen && !sw.At.After(tk.at)) {
		return nil, nil
	}
	tk.seen = true
	tk.at = sw.At
	kwh, meanC, sig, ok := t.energy(sw)
	tk.probesOK = ok
	if !ok || sig != tk.sig {
		tk.samples = tk.samples[:0]
	}
	tk.sig = sig
	if ok {
		tk.samples = append(tk.samples, sample{at: sw.At, kwh: kwh, meanC: meanC})
	}
	from := sw.At.Add(-t.cfg.PowerWindow)
	if settled := t.drawEnd.Add(t.cfg.PowerSettle); t.drawEnded && !t.drawRunning && sw.At.After(settled) && !from.After(settled) {
		// Past the settle, its samples only hold the window back from
		// clearing: drop them, as for a probe-set change.
		from = settled.Add(time.Nanosecond)
	}
	i := 0
	for i < len(tk.samples) && tk.samples[i].at.Before(from) {
		i++
	}
	tk.samples = append(tk.samples[:0], tk.samples[i:]...)
	tk.power = nil
	if p, ok := slope(tk.samples, t.cfg.PowerMinSamples, t.cfg.PowerMinSpan); ok {
		tk.power = &p
		tk.powerFrom = tk.samples[0].at
		if t.clear(tk.powerFrom) {
			t.judge(tk)
		}
	}
	t.judgeAfterDraw(tk, sw.At)
	ev := t.evaluate(nil, tk, sw.At)
	t.fit(tk)
	return ev, nil
}

// Blind takes a sweep of tank id at at that couldn't be estimated (its
// probes unreadable): the tank knows nothing from then, so its state is
// unknown at once, not only once its last good sweep goes stale, and a
// heating spell ends (sweeney/washhouse#17: a dead bus left the right tank
// "heating" for 9 minutes). Skipped as Sweep skips: unsynced, or no later
// than the tank's last.
func (t *Tracker) Blind(id string, at time.Time, clockSynced bool) ([]Event, error) {
	tk := t.tanks[id]
	if tk == nil {
		return nil, fmt.Errorf("thermal: sweep from %q, which isn't configured", id)
	}
	if !clockSynced || (tk.seen && !at.After(tk.at)) {
		return nil, nil
	}
	tk.seen, tk.at, tk.probesOK = true, at, false
	tk.samples = tk.samples[:0]
	tk.power = nil
	return t.evaluate(nil, tk, at), nil
}

// fit adds the latest sweep to tk's loss fit if it is evidence: the tank
// cooling, quiet for UAQuiet, its power window clear of draws, the room
// temperature fresh, and the tank at least UAMinDeltaK above it. The sample
// is loss/ΔT with loss = −dE/dt, weighted by the hours since the previous
// sample.
func (t *Tracker) fit(tk *tankTrack) {
	at := tk.at
	n := len(tk.samples)
	ok := tk.state == StateCooling && !tk.quietSince.IsZero() && at.Sub(tk.quietSince) >= t.cfg.UAQuiet &&
		tk.power != nil && t.clear(tk.powerFrom) && n > 0 &&
		t.ambOK && math.Abs(at.Sub(t.ambAt).Hours()) <= t.cfg.AmbientStale.Hours()
	var dT float64
	if ok {
		dT = tk.samples[n-1].meanC - t.ambC
		ok = dT >= t.cfg.UAMinDeltaK
	}
	if !ok {
		tk.lastFit = time.Time{}
		return
	}
	prev := tk.lastFit
	tk.lastFit = at
	if prev.IsZero() || at.Sub(prev) > t.cfg.StaleAfter {
		return
	}
	h := at.Sub(prev).Hours()
	t.decay(tk, at)
	ua := -*tk.power * 1000 / dT
	tk.uaSum += h * ua
	tk.wSum += h
	tk.evidenceH += h
}

// decay forgets tk's fit by exp(−Δt/UATau) since it was last decayed. Old
// evidence and the prior decay alike, so time alone never moves the fit;
// it only makes room for new evidence.
func (t *Tracker) decay(tk *tankTrack, at time.Time) {
	if !tk.uaAt.IsZero() && at.After(tk.uaAt) {
		f := math.Exp(-at.Sub(tk.uaAt).Hours() / t.cfg.UATau.Hours())
		tk.uaSum *= f
		tk.wSum *= f
		tk.priorW *= f
	}
	if tk.uaAt.IsZero() || at.After(tk.uaAt) {
		tk.uaAt = at
	}
}

// judgeAfterDraw judges a tank heating, after a draw and until its power is
// judged again, from the sweeps since DrawSettle after it, if HW is on now
// and they rise at HeatingOnAfterDrawKW or more (Config). HW, on now: the
// coil's afterglow after HW switches off reached 1.5 kW, with CH on too, and
// CH heats neither tank since the right's valve was replaced (tanktemp's
// reviews of #15); a CH-only rise waits for the power.
func (t *Tracker) judgeAfterDraw(tk *tankTrack, at time.Time) {
	if !t.drawEnded || t.drawRunning || (tk.power != nil && t.clear(tk.powerFrom)) || t.hw.sw != boiler.On {
		return // not after a draw, or the power is judged again already
	}
	from := t.drawEnd.Add(t.cfg.DrawSettle)
	var ss []sample
	for _, s := range tk.samples {
		if s.at.After(from) {
			ss = append(ss, s)
		}
	}
	if p, ok := slope(ss, t.cfg.PowerMinSamples, t.cfg.PowerMinSpan); ok && p >= t.cfg.HeatingOnAfterDrawKW {
		tk.heating, tk.decided = true, true
	}
}

// judge decides heating from the power, with hysteresis: heating starts at
// HeatingOnKW and ends below HeatingOffKW.
func (t *Tracker) judge(tk *tankTrack) {
	p := *tk.power
	if tk.heating {
		tk.heating = p >= t.cfg.HeatingOffKW
	} else {
		tk.heating = p >= t.cfg.HeatingOnKW
	}
	tk.decided = true
}

// stale says whether tk's input is too old or too thin to know anything.
func (t *Tracker) stale(tk *tankTrack, at time.Time) bool {
	return !tk.seen || !tk.probesOK || at.Sub(tk.at) > t.cfg.StaleAfter
}

// evaluate settles tk's state at at and appends the events its change
// makes.
func (t *Tracker) evaluate(ev []Event, tk *tankTrack, at time.Time) []Event {
	wasHeating, prev, prevSource := tk.reported, tk.state, tk.source
	state, source := StateUnknown, SourceNone
	switch {
	case t.stale(tk, at):
		tk.decided, tk.heating = false, false
	case t.drawing(at):
		// The heating decision holds through a draw: its loss swamps the
		// power, and is judged again once the window is clear of it.
		state = StateDrawing
	case !tk.decided:
	case tk.heating:
		state, source = StateHeating, t.source(at)
	default:
		state = StateCooling
	}
	tk.state, tk.source, tk.lastEval, tk.reported = state, source, at, tk.heating
	if state == StateHeating {
		tk.episode = source
	}
	switch {
	case state != StateCooling:
		tk.quietSince = time.Time{}
	case prev != StateCooling:
		tk.quietSince = at
	}
	power := t.powerAt(tk, at)
	if state != prev || source != prevSource {
		ev = append(ev, Event{Type: EventStateChanged, DeviceID: tk.cfg.DeviceID, At: at, State: state, Prev: prev, Source: source, PowerKW: power})
	}
	switch {
	case tk.heating && !wasHeating:
		ev = append(ev, Event{Type: EventHeatingStarted, DeviceID: tk.cfg.DeviceID, At: at, State: state, Prev: prev, Source: source, PowerKW: power})
	case !tk.heating && wasHeating:
		ev = append(ev, Event{Type: EventHeatingEnded, DeviceID: tk.cfg.DeviceID, At: at, State: state, Prev: prev, Source: tk.episode, PowerKW: power})
		tk.episode = SourceNone
	}
	return ev
}

// source is what heats a tank that is gaining heat at at: HW if HW is on,
// else CH if CH is on and HW off; with both off, the channel that was on
// within SourceHold (HW first), since the coil's heat keeps reaching the
// probes after the call ends; else none. Which tank the heat goes to is
// never assumed: only a tank gaining heat has a source.
func (t *Tracker) source(at time.Time) Source {
	hold := t.cfg.SourceHold
	switch {
	case t.hw.sw == boiler.On:
		return SourceHW
	case t.hw.sw == boiler.Off && t.ch.sw == boiler.On:
		return SourceCH
	case t.hw.held(at, hold):
		return SourceHW
	case t.hw.sw == boiler.Off && t.ch.held(at, hold):
		return SourceCH
	}
	return SourceNone
}

// powerAt is tk's power as known at at: unknown once the tank is stale.
func (t *Tracker) powerAt(tk *tankTrack, at time.Time) *float64 {
	if tk.power == nil || t.stale(tk, at) || !t.clear(tk.powerFrom) {
		return nil // a window holding a draw or its catch-up measures the draw
	}
	p := *tk.power
	return &p
}

// slope is the least-squares slope of energy against time, in kW, or ok
// false with fewer than minN samples or a span under minSpan.
func slope(ss []sample, minN int, minSpan time.Duration) (float64, bool) {
	if len(ss) < minN || ss[len(ss)-1].at.Sub(ss[0].at) < minSpan {
		return 0, false
	}
	var n, sx, sy, sxx, sxy float64
	for _, s := range ss {
		x := s.at.Sub(ss[0].at).Hours()
		n++
		sx += x
		sy += s.kwh
		sxx += x * x
		sxy += x * s.kwh
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return 0, false
	}
	return (n*sxy - sx*sy) / d, true
}

// Programmer takes the programmer's HW and CH state as of at (from a switch
// event or a heartbeat; Unknown when not known).
func (t *Tracker) Programmer(at time.Time, hw, ch boiler.Switch) []Event {
	t.hw.set(at, hw)
	t.ch.set(at, ch)
	return t.evaluateAll(at)
}

// evaluateAll settles every tank seen so far at at.
func (t *Tracker) evaluateAll(at time.Time) []Event {
	var ev []Event
	for _, id := range t.order {
		tk := t.tanks[id]
		if tk.seen {
			ev = t.evaluate(ev, tk, at)
		}
	}
	return ev
}

// Ambient takes a boiler-room temperature reading. A reading that isn't
// finite is ignored.
func (t *Tracker) Ambient(at time.Time, tempC float64) {
	if !finite(tempC) || (t.ambOK && at.Before(t.ambAt)) {
		return
	}
	t.ambOK, t.ambAt, t.ambC = true, at, tempC
}

// MeteredDraw says a metered draw started (running true) or ended at at:
// the meter consumer's draw_started and draw_ended. The draw is the
// system's: both tanks supply it through the merged outlet.
func (t *Tracker) MeteredDraw(at time.Time, running bool) []Event {
	if running {
		t.drawRunning = true
	} else if t.drawRunning {
		t.drawRunning, t.drawEnded, t.drawEnd = false, true, at
	}
	return t.evaluateAll(at)
}

// Tick advances event time to at without an input, so a tank can go stale.
func (t *Tracker) Tick(at time.Time) []Event { return t.evaluateAll(at) }

// Tank returns a tank's status, or ok false if it isn't configured.
func (t *Tracker) Tank(id string) (Status, bool) {
	tk := t.tanks[id]
	if tk == nil {
		return Status{}, false
	}
	return Status{DeviceID: id, At: tk.at, State: tk.state, Source: tk.source, PowerKW: t.powerAt(tk, tk.lastEval),
		UA: UA{WPerK: tk.uaSum / tk.wSum, EvidenceHours: tk.evidenceH, PriorShare: tk.priorW / tk.wSum}}, true
}
