package behaviour

import (
	"bytes"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/tank"
	"github.com/sweeney/washhouse/internal/thermal"
	"github.com/sweeney/washhouse/internal/vessel"
)

const examplePath = "../../testdata/behaviour.example.json"

// planConfig is internal/engine's planConfig (its helpers_test.go says
// where each value comes from), repeated here so the example file is
// checked against it.
func planConfig() engine.Config {
	const left, right = "hot_water_cylinder_left", "hot_water_cylinder_right"
	probes := func(upper profile.Role) []tank.ProbeConfig {
		return []tank.ProbeConfig{
			{Position: "bottom", Role: profile.RoleInlet, HeightMM: 265},
			{Position: "lower", Role: profile.RoleBulk, HeightMM: 400},
			{Position: "middle", Role: profile.RoleBulk, HeightMM: 825},
			{Position: "upper", Role: upper, HeightMM: 945},
			{Position: "top", Role: profile.RoleOutlet, HeightMM: 1215},
		}
	}
	return engine.Config{
		Meter: meter.Config{DeviceID: "hot_water_meter", DrawEnd: 60 * time.Second, BathFlowLPerMin: 10, BathMinLitres: 20,
			ShowerMinLitres: 20, ShowerMinDuration: 180 * time.Second, ShowerMinFlowLPerMin: 3, CreepClosed: 10 * time.Second,
			CreepWindow: 2700 * time.Second, StaleAfter: 90 * time.Second, UpdateEvery: 60 * time.Second},
		Vessel: vessel.Params{CapacityL: 200, InnerDiameterMM: 460, DomeDepthMM: 60, Z0MM: 150, SliceMM: 1},
		Estimate: estimate.Config{ColdC: 10, MinC: 40, MixC: 40, KWhPerLitreK: 4.186 / 3600, LowFactor: 0.85,
			DrawFromMM: 281, DrawToMM: 1412},
		Tanks: []tank.Config{{DeviceID: left, Probes: probes(profile.RoleBulk), PriorOutletUnderread: 0.16}, {DeviceID: right, Probes: probes(profile.RoleReturn), PriorOutletUnderread: 0.17}},
		Thermal: thermal.Config{PowerWindow: 10 * time.Minute, PowerMinSpan: 5 * time.Minute, PowerMinSamples: 3,
			PowerExclude: []profile.Role{profile.RoleInlet, profile.RoleOutlet}, MinProbes: 2, StaleAfter: 2 * time.Minute,
			HeatingOnKW: 0.5, HeatingOffKW: 0.3, HeatingOnAfterDrawKW: 1.5, DrawSettle: 60 * time.Second, PowerSettle: 12 * time.Minute, SourceHold: 30 * time.Minute,
			UATau: 24 * time.Hour, UAQuiet: 60 * time.Minute, UAMinDeltaK: 5, AmbientStale: 40 * time.Minute,
			Tanks: []thermal.TankConfig{{DeviceID: left, PriorUAWPerK: 4, PriorHours: 1}, {DeviceID: right, PriorUAWPerK: 17, PriorHours: 1}}},
		Draw: draw.Config{Tanks: []string{left, right}, ColdC: 10, KWhPerLitreK: 4.186 / 3600, ShareSettle: 60 * time.Second,
			BalanceSettle: 8 * time.Minute, MaxSweepGap: 2 * time.Minute, MinReliableLitres: 20, GainKWh: 0.05,
			ThermoclineC: 40, Gaps: []draw.Gap{{Low: "lower", High: "middle"}}, OutletSplit: []float64{0.11, 0.89}},
		OutletSplit:            []float64{0.11, 0.89},
		LowHotWaterL:           60,
		LowHotWaterMarginL:     10,
		BoilerHeartbeat:        15 * time.Minute,
		BoilerMissedHeartbeats: 2.5,
		RecentDraws:            50,
		TankFaultAfter:         5 * time.Minute,
		TankRestoreAfter:       2 * time.Minute,
	}
}

func readExample(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExampleConfigIsThePlans(t *testing.T) {
	fc, got, err := Load(strings.NewReader(readExample(t)))
	if err != nil {
		t.Fatal(err)
	}
	if want := planConfig(); !reflect.DeepEqual(got, want) {
		t.Errorf("example config\n got %+v\nwant %+v", got, want)
	}
	if fc.AmbientTopic != "zigbee2mqtt/climate_groundfloor_boiler-room" {
		t.Errorf("ambient topic %q", fc.AmbientTopic)
	}
	if got, want := fc.Fresh(), (Freshness{Ahead: 90 * time.Second, Stale: 90 * time.Second, Silence: 2 * time.Minute}); got != want {
		t.Errorf("freshness %+v, want %+v", got, want)
	}
}

func TestConfigIsStrict(t *testing.T) {
	ex := readExample(t)
	for _, tc := range []struct {
		name     string
		from, to string // replace from with to in the example
		want     string
	}{
		{"unknown key", `"recent_draws": 50,`, `"recent_draws": 50, "colour": "blue",`, "colour"},
		{"unknown nested key", `"slice_mm": 1`, `"slice_mm": 1, "wall_mm": 3`, "wall_mm"},
		{"unknown probe key", `{"position": "top", "role": "outlet", "height_mm": 1215}`,
			`{"position": "top", "role": "outlet", "height_mm": 1215, "rom": "x"}`, "rom"},
		{"duplicate key", `"recent_draws": 50,`, `"recent_draws": 50, "recent_draws": 40,`, "duplicate"},
		{"duplicate key ignoring case", `"recent_draws": 50,`, `"recent_draws": 50, "Recent_Draws": 40,`, "duplicate"},
		{"missing key", `"recent_draws": 50,`, ``, "recent_draws"},
		{"missing nested key", `"gain_kwh": 0.05,`, ``, "gain_kwh"},
		{"missing probe key", `{"position": "top", "role": "outlet", "height_mm": 1215}`, `{"position": "top", "role": "outlet"}`, "height_mm"},
		{"integer with a fraction", `"power_min_samples": 3,`, `"power_min_samples": 3.5,`, "power_min_samples"},
		{"duration as a number", `"draw_end": "60s",`, `"draw_end": 60,`, "draw_end"},
		{"bad duration", `"draw_end": "60s",`, `"draw_end": "a minute",`, "draw_end"},
		{"null", `"low_hot_water_l": 60,`, `"low_hot_water_l": null,`, "low_hot_water_l"},
		{"unknown role", `"role": "return"`, `"role": "flow"`, "flow"},
		{"invalid value", `"low_factor": 0.85,`, `"low_factor": 1.5,`, "low factor"},
		{"no tanks", ex[strings.Index(ex, `"tanks": [`):strings.Index(ex, `"meter": {`)], `"tanks": [], `, "no tanks"},
		{"trailing data", "}\n", "} {}\n", "after"},
		{"missing margin", `"low_hot_water_margin_l": 10,`, ``, "low_hot_water_margin_l"},
		{"missing heartbeat", `"boiler_heartbeat": "15m",`, ``, "boiler_heartbeat"},
		{"heartbeat as a number", `"boiler_heartbeat": "15m",`, `"boiler_heartbeat": 900,`, "boiler_heartbeat"},
		{"missing missed heartbeats", `"boiler_missed_heartbeats": 2.5,`, ``, "boiler_missed_heartbeats"},
		{"missing outlet split", `"outlet_split": 0.89,`, ``, "outlet_split"},
		{"split not summing to 1", `"outlet_split": 0.89,`, `"outlet_split": 0.8,`, "OutletSplit"},
		{"missing gaps", `"gaps": [{"low": "lower", "high": "middle"}]`, `"gaps_x": []`, "gaps"},
		{"unknown gap key", `{"low": "lower", "high": "middle"}`, `{"low": "lower", "high": "middle", "mm": 1}`, "mm"},
		{"gap position unknown", `{"low": "lower", "high": "middle"}`, `{"low": "lower", "high": "attic"}`, "attic"},
		{"missing freshness", `"silence": "2m"`, `"silence_x": "2m"`, "silence"},
		{"no silence", `"silence": "2m"`, `"silence": "0s"`, "silence"},
		{"no ahead", `"ahead": "90s"`, `"ahead": "0s"`, "ahead"},
		{"negative stale", `"stale": "90s"`, `"stale": "-1s"`, "stale"},
		{"old gap key", `"thermocline_c": 40,`, `"thermocline_c": 40, "gap_low_mm": 400,`, "gap_low_mm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(ex, tc.from) {
				t.Fatalf("%q isn't in the example", tc.from)
			}
			s := strings.Replace(ex, tc.from, tc.to, 1)
			if tc.name == "trailing data" {
				s = strings.TrimRight(ex, "\n") + " {}\n"
			}
			_, _, err := Load(strings.NewReader(s))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// The tank fault delays (sweeney/washhouse#17) reach the engine, and must
// be above 0.
func TestTankFaultDelays(t *testing.T) {
	b, err := os.ReadFile("../../testdata/behaviour.example.json")
	if err != nil {
		t.Fatal(err)
	}
	_, ec, err := Load(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if ec.TankFaultAfter != 5*time.Minute || ec.TankRestoreAfter != 2*time.Minute {
		t.Errorf("fault after %v, restore after %v; want the example's 5m and 2m", ec.TankFaultAfter, ec.TankRestoreAfter)
	}
	for _, bad := range []string{`"tank_fault_after": "0s"`, `"tank_restore_after": "-1s"`} {
		key, _, _ := strings.Cut(bad, ":")
		doc := regexp.MustCompile(key+`: "[^"]*"`).ReplaceAllString(string(b), bad)
		if _, _, err := Load(strings.NewReader(doc)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
