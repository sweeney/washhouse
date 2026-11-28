package influx

import (
	"reflect"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/publish"
)

// Copies of common/wire/washhouse's goldens (v0.1.0): what washhouse
// publishes, so what the points are made from.
const (
	tankCooling  = `{"schema":"net.swee.washhouse.tank.v1","device_id":"hot_water_cylinder_right","timestamp":"2001-01-01T00:36:32.000Z","sweep_at":"2001-01-01T00:36:32.000Z","state":"cooling","heat_source":null,"stored_kwh":8.315,"mean_c":47.906,"top_c":61.375,"hot_l":140.214,"usable_l40":184.331,"usable_l40_low":156.681,"power_kw":-1.33,"ua_w_per_k":17,"probes_ok":5,"probes_total":5}`
	tankUnknown  = `{"schema":"net.swee.washhouse.tank.v1","device_id":"hot_water_cylinder_left","timestamp":"2001-01-01T00:00:00.000Z","sweep_at":null,"state":"unknown","heat_source":null,"stored_kwh":null,"mean_c":null,"top_c":null,"hot_l":null,"usable_l40":null,"usable_l40_low":null,"power_kw":null,"ua_w_per_k":4,"probes_ok":null,"probes_total":null}`
	tankHeating  = `{"schema":"net.swee.washhouse.tank.v1","device_id":"hot_water_cylinder_left","timestamp":"2001-01-01T00:00:30.000Z","sweep_at":"2001-01-01T00:00:30.000Z","state":"heating","heat_source":"hw","stored_kwh":5,"mean_c":40,"top_c":50,"hot_l":90,"usable_l40":100,"usable_l40_low":85,"power_kw":12.1,"ua_w_per_k":4,"probes_ok":4,"probes_total":5}`
	stateDraw    = `{"schema":"net.swee.washhouse.state.v1","timestamp":"2001-01-01T00:17:43.000Z","stored_kwh":21.79,"usable_l40":263.5,"usable_l40_low":223.975,"low_hot_water":false,"boiler":{"hw":false,"ch":false},"ambient_c":25.7,"active_draw":{"draw_id":"20001231T235443Z-76","started_at":"2001-01-01T00:10:10.308Z","last_litre_at":"2001-01-01T00:17:40.112Z","litres":48,"flow_l_min":6.3,"kind":"shower"},"meter":{"timestamp":"2001-01-01T00:17:43.000Z","counting_since":"2000-12-31T23:54:43.000Z","count":54,"litres_since_start":54,"continuous_flow":null},"tanktemp":{"timestamp":"2001-01-01T00:09:43.000Z","online":true,"version":"4aba4d5"},"tanks":[{"device_id":"hot_water_cylinder_left","state":"drawing","heat_source":null,"stored_kwh":11.05,"usable_l40":205.8,"top_c":63.5},{"device_id":"hot_water_cylinder_right","state":"drawing","heat_source":null,"stored_kwh":10.74,"usable_l40":151.2,"top_c":61.9}]}`
	stateUnknown = `{"schema":"net.swee.washhouse.state.v1","timestamp":"2001-01-01T00:00:00.000Z","stored_kwh":null,"usable_l40":null,"usable_l40_low":null,"low_hot_water":null,"boiler":{"hw":null,"ch":null},"ambient_c":null,"active_draw":null,"meter":{"timestamp":null,"counting_since":null,"count":null,"litres_since_start":null,"continuous_flow":null},"tanktemp":{"timestamp":null,"online":null,"version":null},"tanks":[{"device_id":"hot_water_cylinder_left","state":"unknown","heat_source":null,"stored_kwh":null,"usable_l40":null,"top_c":null},{"device_id":"hot_water_cylinder_right","state":"unknown","heat_source":null,"stored_kwh":null,"usable_l40":null,"top_c":null}]}`
	stateCreep   = `{"schema":"net.swee.washhouse.state.v1","timestamp":"2001-01-01T00:30:00.000Z","stored_kwh":null,"usable_l40":null,"usable_l40_low":null,"low_hot_water":null,"boiler":{"hw":null,"ch":null},"ambient_c":null,"active_draw":null,"meter":{"timestamp":null,"counting_since":null,"count":null,"litres_since_start":null,"continuous_flow":{"since":"2001-01-01T00:01:05.217Z","last":"2001-01-01T00:14:11.903Z","litres":2,"flow_l_min":0.082}},"tanktemp":{"timestamp":null,"online":false,"version":null},"tanks":[]}`
	drawEnded    = `{"schema":"net.swee.washhouse.event.v1","event":"draw_ended","timestamp":"2001-01-01T00:18:54.000Z","draw_ended":{"draw_id":"20001231T235443Z-76","started_at":"2001-01-01T00:10:10.308Z","ended_at":"2001-01-01T00:17:46.608Z","duration_s":456.3,"litres":50,"volume_kind":"metered","flow_l_min":6.297,"kind":"shower","meter_from":{"counting_since":"2000-12-31T23:54:43.000Z","seq":76},"meter_to":{"counting_since":"2000-12-31T23:54:43.000Z","seq":220}}}`
	drawBalanced = `{"schema":"net.swee.washhouse.event.v1","event":"draw_balanced","timestamp":"2001-01-01T00:25:54.000Z","draw_balanced":{"draw_id":"20001231T235443Z-76","ended_at":"2001-01-01T00:17:46.608Z","tanks":[{"device_id":"hot_water_cylinder_left","share":0.107,"litres":5.338,"outlet_peak_c":65.25,"inlet_drop_c":20.75},{"device_id":"hot_water_cylinder_right","share":0.893,"litres":44.662,"outlet_peak_c":62,"inlet_drop_c":24.5}],"outlet_peak_c":62.347,"inlet_drop_c":24.5,"energy_kwh":2.924,"heat_balance_l":48.041,"heat_balance_reliable":true,"reasons":[],"masked_by_reheat":false}}`
	drawMasked   = `{"schema":"net.swee.washhouse.event.v1","event":"draw_balanced","timestamp":"2001-01-01T00:23:56.000Z","draw_balanced":{"draw_id":"20001231T150913Z-1784","ended_at":"2001-01-01T00:17:05.708Z","tanks":[{"device_id":"hot_water_cylinder_left","outlet_peak_c":71.938,"inlet_drop_c":35.438},{"device_id":"hot_water_cylinder_right","outlet_peak_c":68,"inlet_drop_c":null}],"outlet_peak_c":70.235,"inlet_drop_c":35.438,"energy_kwh":4.012,"heat_balance_l":57.278,"heat_balance_reliable":false,"reasons":["hw_on","thermocline_gap","interrupted","shares_early"],"masked_by_reheat":true}}`
	flowEnded    = `{"schema":"net.swee.washhouse.event.v1","event":"continuous_flow_ended","timestamp":"2001-01-01T01:59:11.903Z","continuous_flow":{"since":"2001-01-01T00:01:05.217Z","last":"2001-01-01T01:14:11.903Z","litres":6,"flow_l_min":0.082}}`
	drawStarted  = `{"schema":"net.swee.washhouse.event.v1","event":"draw_started","timestamp":"2001-01-01T00:10:43.000Z","active_draw":{"draw_id":"20001231T235443Z-76","started_at":"2001-01-01T00:10:10.308Z","last_litre_at":"2001-01-01T00:10:10.308Z","litres":1,"flow_l_min":null,"kind":"tap"}}`
	heating      = `{"schema":"net.swee.washhouse.event.v1","event":"heating_started","timestamp":"2001-01-01T00:00:30.000Z","tank":{"device_id":"hot_water_cylinder_left","state":"heating","prev":"cooling","heat_source":"hw","power_kw":12.1}}`
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestPoints(t *testing.T) {
	m := Mapper{Base: "water/hot", Site: "home"}
	for _, tc := range []struct {
		name string
		msg  publish.Message
		want []Point
	}{
		{"a tank", publish.Message{Topic: "water/hot/tank/hot_water_cylinder_right", Payload: []byte(tankCooling), Retained: true}, []Point{{
			Measurement: "hot_water_tank",
			Tags:        map[string]string{"site": "home", "device_id": "hot_water_cylinder_right"},
			Fields: map[string]any{"state": "cooling", "stored_kwh": 8.315, "mean_c": 47.906, "top_c": 61.375,
				"hot_l": 140.214, "usable_l40": 184.331, "usable_l40_low": 156.681, "power_kw": -1.33,
				"ua_w_per_k": 17.0, "probes_ok": int64(5), "probes_total": int64(5), "blind": int64(0)},
			Time: at("2001-01-01T00:36:32Z"),
		}}},
		{"a heating tank: its source", publish.Message{Topic: "water/hot/tank/hot_water_cylinder_left", Payload: []byte(tankHeating)}, []Point{{
			Measurement: "hot_water_tank",
			Tags:        map[string]string{"site": "home", "device_id": "hot_water_cylinder_left"},
			Fields: map[string]any{"state": "heating", "heat_source": "hw", "stored_kwh": 5.0, "mean_c": 40.0, "top_c": 50.0,
				"hot_l": 90.0, "usable_l40": 100.0, "usable_l40_low": 85.0, "power_kw": 12.1,
				"ua_w_per_k": 4.0, "probes_ok": int64(4), "probes_total": int64(5), "blind": int64(0)},
			Time: at("2001-01-01T00:00:30Z"),
		}}},
		{"an unknown tank: its nulls left out", publish.Message{Topic: "water/hot/tank/hot_water_cylinder_left", Payload: []byte(tankUnknown)}, []Point{{
			Measurement: "hot_water_tank",
			Tags:        map[string]string{"site": "home", "device_id": "hot_water_cylinder_left"},
			Fields:      map[string]any{"state": "unknown", "ua_w_per_k": 4.0, "blind": int64(0)},
			Time:        at("2001-01-01T00:00:00Z"),
		}}},
		{"a blind tank", publish.Message{Topic: "water/hot/tank/hot_water_cylinder_right", Payload: []byte(tankBlind)}, []Point{{
			Measurement: "hot_water_tank",
			Tags:        map[string]string{"site": "home", "device_id": "hot_water_cylinder_right"},
			Fields: map[string]any{"state": "unknown", "ua_w_per_k": 17.0, "probes_ok": int64(0), "probes_total": int64(5),
				"blind": int64(1), "fault_reason": "probes"},
			Time: at("2001-01-01T00:35:17Z"),
		}}},
		{"the state with a tank blind: the partial, apart", publish.Message{Topic: "water/hot/state", Payload: []byte(statePartial)}, []Point{{
			Measurement: "hot_water_system",
			Tags:        map[string]string{"site": "home"},
			Fields: map[string]any{"low_hot_water": int64(0), "boiler_hw": int64(0), "boiler_ch": int64(1), "ambient_c": 19.4,
				"drawing": int64(0), "continuous_flow": int64(0), "tanktemp_online": int64(1), "tanks_missing": int64(1),
				"partial_stored_kwh": 11.275, "partial_usable_l40": 307.311, "partial_usable_l40_low": 261.214},
			Time: at("2001-01-01T00:30:47Z"),
		}}},
		{"the state, mid-draw", publish.Message{Topic: "water/hot/state", Payload: []byte(stateDraw)}, []Point{{
			Measurement: "hot_water_system",
			Tags:        map[string]string{"site": "home"},
			Fields: map[string]any{"stored_kwh": 21.79, "usable_l40": 263.5, "usable_l40_low": 223.975,
				"low_hot_water": int64(0), "boiler_hw": int64(0), "boiler_ch": int64(0), "ambient_c": 25.7,
				"drawing": int64(1), "continuous_flow": int64(0), "tanktemp_online": int64(1), "tanks_missing": int64(0)},
			Time: at("2001-01-01T00:17:43Z"),
		}}},
		{"the state, all unknown", publish.Message{Topic: "water/hot/state", Payload: []byte(stateUnknown)}, []Point{{
			Measurement: "hot_water_system",
			Tags:        map[string]string{"site": "home"},
			Fields:      map[string]any{"drawing": int64(0), "continuous_flow": int64(0), "tanks_missing": int64(2)},
			Time:        at("2001-01-01T00:00:00Z"),
		}}},
		{"the state, creeping", publish.Message{Topic: "water/hot/state", Payload: []byte(stateCreep)}, []Point{{
			Measurement: "hot_water_system",
			Tags:        map[string]string{"site": "home"},
			Fields:      map[string]any{"drawing": int64(0), "continuous_flow": int64(1), "tanktemp_online": int64(0), "tanks_missing": int64(0)},
			Time:        at("2001-01-01T00:30:00Z"),
		}}},
		{"draw_ended", publish.Message{Topic: "water/hot/events", Payload: []byte(drawEnded), QoS: 1}, []Point{{
			Measurement: "hot_water_draw",
			Tags:        map[string]string{"site": "home"},
			Fields: map[string]any{"draw_id": "20001231T235443Z-76", "kind": "shower", "litres": 50.0,
				"duration_s": 456.3, "flow_l_min": 6.297},
			Time: at("2001-01-01T00:17:46.608Z"),
		}}},
		{"draw_balanced: the same row, and each tank's", publish.Message{Topic: "water/hot/events", Payload: []byte(drawBalanced), QoS: 1}, []Point{{
			Measurement: "hot_water_draw",
			Tags:        map[string]string{"site": "home"},
			Fields: map[string]any{"draw_id": "20001231T235443Z-76", "outlet_peak_c": 62.347, "inlet_drop_c": 24.5,
				"energy_kwh": 2.924, "heat_balance_l": 48.041, "heat_balance_reliable": int64(1),
				"masked_by_reheat": int64(0), "reasons": ""},
			Time: at("2001-01-01T00:17:46.608Z"),
		}, {
			Measurement: "hot_water_draw_tank",
			Tags:        map[string]string{"site": "home", "device_id": "hot_water_cylinder_left"},
			Fields: map[string]any{"draw_id": "20001231T235443Z-76", "share": 0.107, "litres": 5.338,
				"outlet_peak_c": 65.25, "inlet_drop_c": 20.75},
			Time: at("2001-01-01T00:17:46.608Z"),
		}, {
			Measurement: "hot_water_draw_tank",
			Tags:        map[string]string{"site": "home", "device_id": "hot_water_cylinder_right"},
			Fields: map[string]any{"draw_id": "20001231T235443Z-76", "share": 0.893, "litres": 44.662,
				"outlet_peak_c": 62.0, "inlet_drop_c": 24.5},
			Time: at("2001-01-01T00:17:46.608Z"),
		}}},
		{"draw_balanced, masked: no shares", publish.Message{Topic: "water/hot/events", Payload: []byte(drawMasked), QoS: 1}, []Point{{
			Measurement: "hot_water_draw",
			Tags:        map[string]string{"site": "home"},
			Fields: map[string]any{"draw_id": "20001231T150913Z-1784", "outlet_peak_c": 70.235, "inlet_drop_c": 35.438,
				"energy_kwh": 4.012, "heat_balance_l": 57.278, "heat_balance_reliable": int64(0),
				"masked_by_reheat": int64(1), "reasons": "hw_on,thermocline_gap,interrupted,shares_early"},
			Time: at("2001-01-01T00:17:05.708Z"),
		}, {
			Measurement: "hot_water_draw_tank",
			Tags:        map[string]string{"site": "home", "device_id": "hot_water_cylinder_left"},
			Fields:      map[string]any{"draw_id": "20001231T150913Z-1784", "outlet_peak_c": 71.938, "inlet_drop_c": 35.438},
			Time:        at("2001-01-01T00:17:05.708Z"),
		}, {
			Measurement: "hot_water_draw_tank",
			Tags:        map[string]string{"site": "home", "device_id": "hot_water_cylinder_right"},
			Fields:      map[string]any{"draw_id": "20001231T150913Z-1784", "outlet_peak_c": 68.0},
			Time:        at("2001-01-01T00:17:05.708Z"),
		}}},
		{"continuous_flow_ended", publish.Message{Topic: "water/hot/events", Payload: []byte(flowEnded), QoS: 1}, []Point{{
			Measurement: "hot_water_continuous_flow",
			Tags:        map[string]string{"site": "home"},
			Fields:      map[string]any{"litres": 6.0, "flow_l_min": 0.082, "duration_s": 4386.686},
			Time:        at("2001-01-01T00:01:05.217Z"),
		}}},
		// The rest are in the state and the tanks already.
		{"draw_started", publish.Message{Topic: "water/hot/events", Payload: []byte(drawStarted), QoS: 1}, nil},
		{"heating_started", publish.Message{Topic: "water/hot/events", Payload: []byte(heating), QoS: 1}, nil},
		{"status", publish.Message{Topic: "water/hot/status", Payload: []byte(`{"schema":"net.swee.washhouse.status.v1"}`)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.Points(tc.msg)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// No site: no site tag at all (an empty tag value is its own series).
func TestPointsWithoutASite(t *testing.T) {
	got, err := Mapper{Base: "water/hot"}.Points(publish.Message{Topic: "water/hot/state", Payload: []byte(stateUnknown)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Tags) != 0 {
		t.Errorf("tags %+v", got)
	}
	got, err = Mapper{Base: "water/hot"}.Points(publish.Message{Topic: "water/hot/tank/x", Payload: []byte(tankUnknown)})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"device_id": "hot_water_cylinder_left"}; len(got) != 1 || !reflect.DeepEqual(got[0].Tags, want) {
		t.Errorf("tags %+v", got)
	}
}

func TestPointsRefuse(t *testing.T) {
	m := Mapper{Base: "water/hot"}
	for _, msg := range []publish.Message{
		{Topic: "water/hot/state", Payload: []byte(`{`)},
		{Topic: "water/hot/tank/a", Payload: []byte(`{"schema":"x"}`)},
		{Topic: "water/hot/events", Payload: []byte(`[]`)},
		{Topic: "elsewhere/state", Payload: []byte(stateUnknown)},
	} {
		if p, err := m.Points(msg); err == nil {
			t.Errorf("%s %s: %+v", msg.Topic, msg.Payload, p)
		}
	}
}

// WIRE.md's blind-cylinder examples (common/wire's tank-fault and
// state-partial goldens).
const tankBlind = `{"schema":"net.swee.washhouse.tank.v1","device_id":"hot_water_cylinder_right","timestamp":"2001-01-01T00:35:17.000Z","sweep_at":"2001-01-01T00:35:17.000Z","state":"unknown","heat_source":null,"stored_kwh":null,"mean_c":null,"top_c":null,"hot_l":null,"usable_l40":null,"usable_l40_low":null,"power_kw":null,"ua_w_per_k":17,"probes_ok":0,"probes_total":5,"fault":{"since":"2001-01-01T00:30:17.000Z","reason":"probes","errors":{"not_present":5}}}`

const statePartial = `{"schema":"net.swee.washhouse.state.v1","timestamp":"2001-01-01T00:30:47.000Z","stored_kwh":null,"usable_l40":null,"usable_l40_low":null,"low_hot_water":false,"partial":{"missing":["hot_water_cylinder_right"],"stored_kwh":11.275,"usable_l40":307.311,"usable_l40_low":261.214},"boiler":{"hw":false,"ch":true},"ambient_c":19.4,"active_draw":null,"meter":{"timestamp":"2001-01-01T00:30:47.000Z","counting_since":"2000-12-31T23:54:43.000Z","count":54,"litres_since_start":54,"continuous_flow":null},"tanktemp":{"timestamp":"2001-01-01T00:09:43.000Z","online":true,"version":"4aba4d5"},"tanks":[{"device_id":"hot_water_cylinder_left","state":"cooling","heat_source":null,"stored_kwh":11.275,"usable_l40":307.311,"top_c":63.875},{"device_id":"hot_water_cylinder_right","state":"unknown","heat_source":null,"stored_kwh":null,"usable_l40":null,"top_c":null}]}`
