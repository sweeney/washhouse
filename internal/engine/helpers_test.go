package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/tank"
	"github.com/sweeney/washhouse/internal/thermal"
	"github.com/sweeney/washhouse/internal/vessel"
	"github.com/sweeney/washhouse/internal/wire/tanktemp"
)

const (
	left  = "hot_water_cylinder_left"
	right = "hot_water_cylinder_right"
)

// planConfig is the plan's values, copied from the other packages' test
// helpers (each says where its values come from):
//   - Meter: internal/meter's planConfig, plus UpdateEvery 60 s, which
//     this package chose (meter's tests leave draw_updated off): statehouse
//     refreshes a draw's expiry on it, and once a minute is plenty against
//     its 30-minute safety.
//   - Vessel and Estimate: internal/thermal's testEstimator (avail.py's
//     STI 210, drawable from the cold inlet to the hot outlet).
//   - Tanks: internal/tank's houseTanks.
//   - Thermal: internal/thermal's planConfig.
//   - Draw: internal/draw's testConfig.
//   - OutletSplit 0.11 left, 0.89 right: the right tank's share of the
//     50 L layered shower, from the balance sweep (internal/draw's
//     TestFixtureShowerLayered: 0.893; the 12 L evening tap reads 0.902),
//     the only unmasked multi-litre draws with a clean before sweep.
//   - LowHotWaterL 60: this package's choice, from the explainer's
//     simulation (docs/washhouse-explainer.html, low_hot_water under 60 L
//     of usable_l40_low). PLAN.md gives no number; provisional, for the
//     owner. LowHotWaterMarginL 10: this package's choice, about one
//     sweep-to-sweep wobble of a tank in a draw's first minutes, several
//     times over.
//   - BoilerHeartbeat 15 min: boiler-sensor's heartbeat_ms (900000) in
//     its wire-contract test and the fixtures' spacing; the fixtures strip
//     the config block that carries it. BoilerMissedHeartbeats 2.5:
//     tanktemp's review of PR #2.
//   - RecentDraws 50: this package's choice; a day is a few dozen draws.
func planConfig() Config {
	probes := func(upper profile.Role) []tank.ProbeConfig {
		return []tank.ProbeConfig{
			{Position: "bottom", Role: profile.RoleInlet, HeightMM: 265},
			{Position: "lower", Role: profile.RoleBulk, HeightMM: 400},
			{Position: "middle", Role: profile.RoleBulk, HeightMM: 825},
			{Position: "upper", Role: upper, HeightMM: 945},
			{Position: "top", Role: profile.RoleOutlet, HeightMM: 1215},
		}
	}
	return Config{
		Meter: meter.Config{
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
			UpdateEvery:          60 * time.Second,
		},
		Vessel: vessel.Params{CapacityL: 200, InnerDiameterMM: 460, DomeDepthMM: 60, Z0MM: 150, SliceMM: 1},
		Estimate: estimate.Config{ColdC: 10, MinC: 40, MixC: 40, KWhPerLitreK: 4.186 / 3600,
			LowFactor: 0.85, DrawFromMM: 281, DrawToMM: 1237},
		Tanks: []tank.Config{
			{DeviceID: left, Probes: probes(profile.RoleBulk)},
			{DeviceID: right, Probes: probes(profile.RoleReturn)},
		},
		Thermal: thermal.Config{
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
			Tanks: []thermal.TankConfig{
				{DeviceID: left, PriorUAWPerK: 4, PriorHours: 1},
				{DeviceID: right, PriorUAWPerK: 17, PriorHours: 1},
			},
		},
		Draw: draw.Config{
			Tanks:             []string{left, right},
			ColdC:             10,
			KWhPerLitreK:      4.186 / 3600,
			ShareSettle:       60 * time.Second,
			BalanceSettle:     8 * time.Minute,
			MaxSweepGap:       2 * time.Minute,
			MinReliableLitres: 20,
			GainKWh:           0.05,
			ThermoclineC:      40,
			Gaps:              []draw.Gap{{Low: "lower", High: "middle"}},
		},
		OutletSplit:            []float64{0.11, 0.89},
		LowHotWaterL:           60,
		LowHotWaterMarginL:     10,
		BoilerHeartbeat:        15 * time.Minute,
		BoilerMissedHeartbeats: 2.5,
		RecentDraws:            50,
	}
}

func newEngine(t testing.TB, cfg Config) *Engine {
	t.Helper()
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

var t0 = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// sweep is a tanktemp state of tank id at at with each probe's
// temperature, bottom first; a nil temperature is a failed read.
func sweep(id string, at time.Time, temps ...*float64) tanktemp.State {
	pos := []string{"bottom", "lower", "middle", "upper", "top"}
	hts := []int{265, 400, 825, 945, 1215}
	st := tanktemp.State{DeviceID: id, Timestamp: at, ClockSynced: true, SweepMS: 900}
	for k, p := range pos {
		h := hts[k]
		sn := tanktemp.Sensor{Position: p, Order: k + 1, HeightMM: &h, TempC: temps[k], RawC: temps[k]}
		if temps[k] == nil {
			e := "crc_mismatch"
			sn.Error = &e
		}
		st.Sensors = append(st.Sensors, sn)
	}
	return st
}

// uniform is tank id at at with every probe at c.
func uniform(id string, at time.Time, c float64) tanktemp.State {
	return sweep(id, at, &c, &c, &c, &c, &c)
}

func f(x float64) *float64 { return &x }

func ofType(ev []Event, ty EventType) []Event {
	var out []Event
	for _, e := range ev {
		if e.Type == ty {
			out = append(out, e)
		}
	}
	return out
}

func fm(p *float64, format string) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf(format, *p)
}
