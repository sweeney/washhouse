package thermal

import (
	"testing"
	"time"

	"github.com/sweeney/common/wire/tanktemp"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/tank"
	"github.com/sweeney/washhouse/internal/vessel"
)

const (
	left  = "hot_water_cylinder_left"
	right = "hot_water_cylinder_right"
)

// planConfig is the thresholds the tests run with. Where each comes from:
//   - PowerWindow 10 min, PowerMinSpan 5 min, PowerMinSamples 3: sweeps come
//     every 10–30 s with probes quantised to 1/16 °C (≈0.015 kWh per probe
//     step on the STI 210), so a 10-minute least-squares slope is steady to
//     about ±0.2 kW at rest, while a reheat shows within minutes.
//   - PowerExclude inlet and outlet: probe 1 sits on the cold inlet and
//     probe 5 on the hot outlet (HOT-WATER-SYSTEM.md §1); both answer to
//     flow, sub-litre draws included, rather than to heat in the water. With
//     them in, ch-only-no-heat shows +1.5–2 kW "gains" on the right tank
//     after small draws the meter can't count; without them it shows none,
//     and the reheats are unchanged.
//   - MinProbes 2: a profile needs two probes.
//   - StaleAfter 2 min: four missed 30 s sweeps.
//   - HeatingOnKW 0.5, HeatingOffKW 0.3: at rest the bulk power stays within
//     about ±0.25 kW (the left tank in the 8 Nov CH-only spell peaked at
//     0.17 kW), and the weakest real reheat, the left tank's share with HW
//     and CH both on, passes 0.5 kW. The gap is hysteresis.
//   - DrawSettle 60 s: probe 1 sees a draw 1–43 s after its litres (PLAN.md
//     lessons §5), and the meter's draw already ends 60 s after its last
//     litre.
//   - PowerSettle 12 min: after the 50 L shower the right tank's bulk
//     energy kept falling for 10-12 minutes (9.02 -> 8.06 kWh) with no flow:
//     the inlet plume settling and the probes' lag, not loss. Empirical.
//   - SourceHold 30 min: heat keeps reaching the probes after the programmer
//     switches off (8 Nov: the right tank gained for about 17 minutes after
//     CH_OFF; reheat-morning: still gaining 10 minutes after HW_OFF).
//   - UATau 24 h: evidence from a night is about 7 hours; a day without
//     evidence decays it by e, so the next night carries about 3/4 of the fit
//     and two nights about 9/10 (PLAN.md: re-converge within a night or two).
//   - UAQuiet 60 min: the loss protocol starts an hour after the last draw
//     (HOT-WATER-SYSTEM.md §8); the same after a reheat, while its heat
//     spreads.
//   - UAMinDeltaK 5 K: below that, the room sensor's 1 °C steps are 20%.
//   - AmbientStale 40 min: the boiler-room sensor reports about every
//     15 minutes, with some missing.
//   - priors: left 4 W/K and right 17 W/K (HOT-WATER-SYSTEM.md §3), each
//     worth one hour, so a single night outweighs it.
func planConfig() Config {
	return Config{
		PowerWindow:     10 * time.Minute,
		PowerMinSpan:    5 * time.Minute,
		PowerMinSamples: 3,
		PowerExclude:    []profile.Role{profile.RoleInlet, profile.RoleOutlet},
		MinProbes:       2,
		StaleAfter:      2 * time.Minute,
		HeatingOnKW:     0.5,
		HeatingOffKW:    0.3,
		DrawSettle:      60 * time.Second,
		PowerSettle:     12 * time.Minute,
		SourceHold:      30 * time.Minute,
		UATau:           24 * time.Hour,
		UAQuiet:         60 * time.Minute,
		UAMinDeltaK:     5,
		AmbientStale:    40 * time.Minute,
		Tanks: []TankConfig{
			{DeviceID: left, PriorUAWPerK: 4, PriorHours: 1},
			{DeviceID: right, PriorUAWPerK: 17, PriorHours: 1},
		},
	}
}

// meterConfig is internal/meter's plan config (its helpers_test.go).
func meterConfig() meter.Config {
	return meter.Config{
		DeviceID:             "hot_water_meter",
		DrawEnd:              60 * time.Second,
		BathFlowLPerMin:      10,
		BathMinLitres:        20,
		ShowerMinLitres:      20,
		ShowerMinDuration:    180 * time.Second,
		ShowerMinFlowLPerMin: 3,
		CreepClosed:          10 * time.Second,
		CreepWindow:          2700 * time.Second,
		StaleAfter:           90 * time.Second,
	}
}

// testEstimator is avail.py's STI 210 vessel, drawable from the cold inlet
// to the hot outlet.
func testEstimator(t testing.TB) *estimate.Estimator {
	t.Helper()
	v, err := vessel.New(vessel.Params{CapacityL: 200, InnerDiameterMM: 460, DomeDepthMM: 60, Z0MM: 150, SliceMM: 1})
	if err != nil {
		t.Fatal(err)
	}
	e, err := estimate.New(v, estimate.Config{ColdC: 10, MinC: 40, MixC: 40, KWhPerLitreK: 4.186 / 3600,
		LowFactor: 0.85, DrawFromMM: 281, DrawToMM: 1237})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// houseTanks is internal/tank's fixture layout: five probes each at the same
// heights; the right tank's upper probe is on the secondary return.
func houseTanks() map[string]tank.Config {
	probes := func(upper profile.Role) []tank.ProbeConfig {
		return []tank.ProbeConfig{
			{Position: "bottom", Role: profile.RoleInlet, HeightMM: 265},
			{Position: "lower", Role: profile.RoleBulk, HeightMM: 400},
			{Position: "middle", Role: profile.RoleBulk, HeightMM: 825},
			{Position: "upper", Role: upper, HeightMM: 945},
			{Position: "top", Role: profile.RoleOutlet, HeightMM: 1215},
		}
	}
	return map[string]tank.Config{
		left:  {DeviceID: left, Probes: probes(profile.RoleBulk)},
		right: {DeviceID: right, Probes: probes(profile.RoleReturn)},
	}
}

func houseTankers(t testing.TB) map[string]*tank.Tank {
	t.Helper()
	est := testEstimator(t)
	out := map[string]*tank.Tank{}
	for id, c := range houseTanks() {
		tk, err := tank.New(c, est)
		if err != nil {
			t.Fatal(err)
		}
		out[id] = tk
	}
	return out
}

func newTracker(t testing.TB, cfg Config) *Tracker {
	t.Helper()
	tr, err := New(cfg, testEstimator(t))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

var t0 = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// kwhPerK is the STI 210's 200 L heat capacity: a uniform tank stores this
// much more per kelvin.
const kwhPerK = 200 * 4.186 / 3600

// synth makes sweeps of uniform tanks: every probe at the same temperature,
// so E = kwhPerK × (T − 10) and dE/dt is exactly the temperature's slope.
type synth struct {
	t     testing.TB
	tanks map[string]*tank.Tank
}

func newSynth(t testing.TB) *synth { return &synth{t: t, tanks: houseTankers(t)} }

// sweep is a uniform tank id at tempC at time at. A nil probe in failed
// marks those positions (0 bottom … 4 top) as failed reads.
func (s *synth) sweep(id string, at time.Time, tempC float64, failed ...int) tank.Sweep {
	s.t.Helper()
	temps := []float64{tempC, tempC, tempC, tempC, tempC}
	return s.sweepT(id, at, temps, failed...)
}

// sweepT is tank id at at with each probe's temperature, bottom first.
func (s *synth) sweepT(id string, at time.Time, temps []float64, failed ...int) tank.Sweep {
	s.t.Helper()
	pos := []string{"bottom", "lower", "middle", "upper", "top"}
	hts := []int{265, 400, 825, 945, 1215}
	st := tanktemp.State{DeviceID: id, Timestamp: at, ClockSynced: true}
	for k, p := range pos {
		h := hts[k]
		v := temps[k]
		sn := tanktemp.Sensor{Position: p, Order: k + 1, HeightMM: &h, TempC: &v, RawC: &v}
		for _, f := range failed {
			if f == k {
				e := "crc_mismatch"
				sn.TempC, sn.RawC, sn.Error = nil, nil, &e
			}
		}
		st.Sensors = append(st.Sensors, sn)
	}
	sw, err := s.tanks[id].Sweep(st)
	if err != nil {
		s.t.Fatal(err)
	}
	return sw
}

// ramp feeds tank id sweeps every step from start for d, its temperature
// starting at fromC and rising at kw (kW, so kw/kwhPerK K per hour), and
// returns the events and the temperature at the end.
func (s *synth) ramp(tr *Tracker, id string, start time.Time, d, step time.Duration, fromC, kw float64) ([]Event, float64) {
	s.t.Helper()
	var ev []Event
	c := fromC
	for at := start; at.Before(start.Add(d)); at = at.Add(step) {
		c = fromC + kw/kwhPerK*at.Sub(start).Hours()
		e, err := tr.Sweep(s.sweep(id, at, c))
		if err != nil {
			s.t.Fatal(err)
		}
		ev = append(ev, e...)
	}
	return ev, c
}

func ofType(ev []Event, ty EventType) []Event {
	var out []Event
	for _, e := range ev {
		if e.Type == ty {
			out = append(out, e)
		}
	}
	return out
}

func mustStatus(t testing.TB, tr *Tracker, id string) Status {
	t.Helper()
	st, ok := tr.Tank(id)
	if !ok {
		t.Fatalf("no status for %s", id)
	}
	return st
}
