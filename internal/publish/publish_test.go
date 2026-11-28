package publish

import (
	"strings"
	"testing"
	"time"

	"github.com/sweeney/common/wire/washhouse"
	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/thermal"
)

var t0 = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func f(x float64) *float64 { return &x }
func bp(x bool) *bool      { return &x }

const left, right = "hot_water_cylinder_left", "hot_water_cylinder_right"

func state(at time.Time) engine.State {
	return engine.State{At: at, Tanks: []engine.TankState{
		{DeviceID: left, State: thermal.StateUnknown, UAWPerK: f(4)},
		{DeviceID: right, State: thermal.StateUnknown, UAWPerK: f(17)},
	}}
}

func topics(ms []Message) string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Topic)
	}
	return strings.Join(out, " ")
}

func newP(t *testing.T) *Publisher {
	t.Helper()
	p, err := New("test/water/hot")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewRefusesABadBase(t *testing.T) {
	for _, base := range []string{"", "/", "water/hot/#", "water/+/x", "water/hot/tanktemp", "water/hot/tanktemp/x"} {
		if _, err := New(base); err == nil {
			t.Errorf("base %q accepted", base)
		}
	}
}

// Nothing goes out before the engine has an input: there is no event time
// to stamp it with.
func TestNothingBeforeTheFirstInput(t *testing.T) {
	ms, err := newP(t).After(state(time.Time{}), nil)
	if err != nil || len(ms) != 0 {
		t.Errorf("got %v, %v", topics(ms), err)
	}
}

// The first publish carries every tank and the state, retained at QoS 0.
func TestFirstPublish(t *testing.T) {
	ms, err := newP(t).After(state(t0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := topics(ms), "test/water/hot/tank/"+left+" test/water/hot/tank/"+right+" test/water/hot/state"; got != want {
		t.Fatalf("topics %q, want %q", got, want)
	}
	for _, m := range ms {
		if !m.Retained || m.QoS != 0 {
			t.Errorf("%s: retained %v qos %d", m.Topic, m.Retained, m.QoS)
		}
	}
	tk, err := washhouse.DecodeTank(ms[0].Payload)
	if err != nil || tk.SweepAt != nil || tk.UAWPerK == nil || *tk.UAWPerK != 4 || !tk.Timestamp.Equal(t0) {
		t.Errorf("tank %+v, %v", tk, err)
	}
	if _, err := washhouse.DecodeState(ms[2].Payload); err != nil {
		t.Error(err)
	}
}

// A topic is published only when its content changes; the timestamp
// moving on alone isn't a change.
func TestOnlyOnChange(t *testing.T) {
	p := newP(t)
	if _, err := p.After(state(t0), nil); err != nil {
		t.Fatal(err)
	}
	ms, _ := p.After(state(t0.Add(30*time.Second)), nil)
	if len(ms) != 0 {
		t.Errorf("unchanged content published: %s", topics(ms))
	}
	// A change only in one tank's power: its message, and not the state,
	// whose summary doesn't carry power.
	s := state(t0.Add(time.Minute))
	s.Tanks[1].PowerKW = f(-0.4)
	ms, _ = p.After(s, nil)
	if got := topics(ms); got != "test/water/hot/tank/"+right {
		t.Errorf("topics %q", got)
	}
	// A change below the wire's 3 decimals isn't a change.
	s = state(t0.Add(90 * time.Second))
	s.Tanks[1].PowerKW = f(-0.4000001)
	if ms, _ = p.After(s, nil); len(ms) != 0 {
		t.Errorf("a sub-millidecimal change published: %s", topics(ms))
	}
	// A change in the totals: the state only.
	s.StoredKWh = f(20)
	ms, _ = p.After(s, nil)
	if got := topics(ms); got != "test/water/hot/state" {
		t.Errorf("topics %q", got)
	}
	st, _ := washhouse.DecodeState(ms[0].Payload)
	if !st.Timestamp.Equal(t0.Add(90 * time.Second)) {
		t.Errorf("state stamped %v, want the engine's time", st.Timestamp)
	}
}

// Events go out first, in order, at QoS 1 and not retained; then the
// tanks, then the state, so the state that drops active_draw follows its
// draw_ended (WIRE.md, State).
func TestEventsFirst(t *testing.T) {
	p := newP(t)
	s := state(t0)
	s.ActiveDraw = &engine.ActiveDraw{DrawID: "x-1", StartedAt: t0, LastLitreAt: t0, LitresSoFar: 1, Kind: meter.KindTap}
	if _, err := p.After(s, nil); err != nil {
		t.Fatal(err)
	}
	s = state(t0.Add(70 * time.Second))
	ended := engine.Event{Type: engine.EventDrawEnded, At: s.At, Ended: &engine.DrawEnded{DrawID: "x-1", StartedAt: t0,
		EndedAt: t0, Litres: 1, VolumeKind: "metered", Kind: meter.KindTap}}
	ms, err := p.After(s, []engine.Event{ended})
	if err != nil {
		t.Fatal(err)
	}
	if got := topics(ms); got != "test/water/hot/events test/water/hot/state" {
		t.Fatalf("topics %q", got)
	}
	if ms[0].Retained || ms[0].QoS != 1 {
		t.Errorf("event retained %v qos %d", ms[0].Retained, ms[0].QoS)
	}
	st, _ := washhouse.DecodeState(ms[1].Payload)
	if st.ActiveDraw != nil {
		t.Error("the state after draw_ended still has the draw")
	}
}

func TestEventMapping(t *testing.T) {
	hw := thermal.SourceHW
	for _, c := range []struct {
		name string
		in   engine.Event
		want string
	}{
		{"draw_started", engine.Event{Type: engine.EventDrawStarted, At: t0, Draw: &engine.ActiveDraw{
			DrawID: "20001231T235443Z-76", StartedAt: t0, LastLitreAt: t0.Add(9 * time.Second), LitresSoFar: 2,
			FlowLPerMin: f(6.6666666), Kind: meter.KindTap}},
			`{"schema":"net.swee.washhouse.event.v1","event":"draw_started","timestamp":"2001-01-01T00:00:00.000Z","active_draw":{"draw_id":"20001231T235443Z-76","started_at":"2001-01-01T00:00:00.000Z","last_litre_at":"2001-01-01T00:00:09.000Z","litres":2,"flow_l_min":6.667,"kind":"tap"}}`},
		{"draw_ended", engine.Event{Type: engine.EventDrawEnded, At: t0.Add(time.Minute), Ended: &engine.DrawEnded{
			DrawID: "a-1", StartedAt: t0, EndedAt: t0.Add(30500 * time.Millisecond), DurationS: 30.5, Litres: 3,
			VolumeKind: "metered", Kind: meter.KindTap,
			From: meter.Ref{CountingSince: t0, Seq: 1}, To: meter.Ref{CountingSince: t0, Seq: 7}}},
			`{"schema":"net.swee.washhouse.event.v1","event":"draw_ended","timestamp":"2001-01-01T00:01:00.000Z","draw_ended":{"draw_id":"a-1","started_at":"2001-01-01T00:00:00.000Z","ended_at":"2001-01-01T00:00:30.500Z","duration_s":30.5,"litres":3,"volume_kind":"metered","flow_l_min":null,"kind":"tap","meter_from":{"counting_since":"2001-01-01T00:00:00.000Z","seq":1},"meter_to":{"counting_since":"2001-01-01T00:00:00.000Z","seq":7}}}`},
		{"draw_balanced masked", engine.Event{Type: engine.EventDrawBalanced, At: t0, Balanced: &engine.DrawBalanced{
			DrawID: "a-1", EndedAt: t0, Tanks: []engine.TankShare{{DeviceID: left, OutletPeakC: f(70)}, {DeviceID: right, Share: nil}},
			EnergyKWh: f(-0.4), HeatBalanceL: f(0), Reasons: []draw.Reason{draw.ReasonHWOn, draw.ReasonGained}, MaskedByReheat: true}},
			`{"schema":"net.swee.washhouse.event.v1","event":"draw_balanced","timestamp":"2001-01-01T00:00:00.000Z","draw_balanced":{"draw_id":"a-1","ended_at":"2001-01-01T00:00:00.000Z","tanks":[{"device_id":"hot_water_cylinder_left","outlet_peak_c":70,"inlet_drop_c":null},{"device_id":"hot_water_cylinder_right","outlet_peak_c":null,"inlet_drop_c":null}],"outlet_peak_c":null,"inlet_drop_c":null,"energy_kwh":-0.4,"heat_balance_l":0,"heat_balance_reliable":false,"reasons":["hw_on","tank_gained"],"masked_by_reheat":true}}`},
		{"draw_balanced shares", engine.Event{Type: engine.EventDrawBalanced, At: t0, Balanced: &engine.DrawBalanced{
			DrawID: "a-1", EndedAt: t0, Tanks: []engine.TankShare{{DeviceID: left, Share: f(0.25), Litres: f(5)}, {DeviceID: right, Share: f(0.75), Litres: f(15)}},
			HeatBalanceReliable: true}},
			`{"schema":"net.swee.washhouse.event.v1","event":"draw_balanced","timestamp":"2001-01-01T00:00:00.000Z","draw_balanced":{"draw_id":"a-1","ended_at":"2001-01-01T00:00:00.000Z","tanks":[{"device_id":"hot_water_cylinder_left","share":0.25,"litres":5,"outlet_peak_c":null,"inlet_drop_c":null},{"device_id":"hot_water_cylinder_right","share":0.75,"litres":15,"outlet_peak_c":null,"inlet_drop_c":null}],"outlet_peak_c":null,"inlet_drop_c":null,"energy_kwh":null,"heat_balance_l":null,"heat_balance_reliable":true,"reasons":[],"masked_by_reheat":false}}`},
		{"continuous_flow_started", engine.Event{Type: engine.EventContinuousFlowStarted, At: t0, Flow: &engine.Flow{
			Since: t0, Last: t0.Add(14 * time.Minute), Litres: 2, FlowLPerMin: 1.0 / 14}},
			`{"schema":"net.swee.washhouse.event.v1","event":"continuous_flow_started","timestamp":"2001-01-01T00:00:00.000Z","continuous_flow":{"since":"2001-01-01T00:00:00.000Z","last":"2001-01-01T00:14:00.000Z","litres":2,"flow_l_min":0.071}}`},
		{"heating_ended", engine.Event{Type: engine.EventHeatingEnded, At: t0, Tank: &engine.TankEvent{
			DeviceID: left, State: thermal.StateCooling, Prev: thermal.StateHeating, Source: &hw, PowerKW: f(0.1)}},
			`{"schema":"net.swee.washhouse.event.v1","event":"heating_ended","timestamp":"2001-01-01T00:00:00.000Z","tank":{"device_id":"hot_water_cylinder_left","state":"cooling","prev":"heating","heat_source":"hw","power_kw":0.1}}`},
	} {
		m, err := Event(c.in, "water/hot")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if m.Topic != "water/hot/events" || string(m.Payload) != c.want {
			t.Errorf("%s:\n got %s %s\nwant water/hot/events %s", c.name, m.Topic, m.Payload, c.want)
		}
	}
}

// An engine share without its litres can't reach the wire half-formed.
func TestEventRefusesAHalfShare(t *testing.T) {
	_, err := Event(engine.Event{Type: engine.EventDrawBalanced, At: t0, Balanced: &engine.DrawBalanced{
		Tanks: []engine.TankShare{{DeviceID: left, Share: f(1)}}}}, "water/hot")
	if err == nil {
		t.Error("a share without litres encoded")
	}
}

func TestStateMapping(t *testing.T) {
	hw := thermal.SourceHW
	sweep := t0.Add(-10 * time.Second)
	count := uint64(54)
	s := engine.State{
		At: t0, StoredKWh: f(21.79), UsableL40: f(263.5), UsableL40Low: f(223.975), LowHotWater: bp(false),
		Boiler: engine.Boiler{HW: bp(true)}, AmbientC: f(25.7),
		ActiveDraw: &engine.ActiveDraw{DrawID: "a-1", StartedAt: t0, LastLitreAt: t0, LitresSoFar: 1, Kind: meter.KindTap},
		Meter: engine.MeterState{At: &t0, Count: &count, CountingSince: &sweep, Litres: f(54),
			ContinuousFlow: &engine.Flow{Since: sweep, Last: t0, Litres: 2, FlowLPerMin: 0.1}},
		Tanktemp: engine.TanktempStatus{Online: bp(false)},
		Tanks: []engine.TankState{{DeviceID: left, At: &sweep, State: thermal.StateHeating, HeatSource: &hw,
			StoredKWh: f(11), MeanC: f(50), TopC: f(63.5), HotL: f(150), UsableL40: f(205.8), UsableL40Low: f(175),
			PowerKW: f(9.8), UAWPerK: f(4), ProbesOK: ip(5), ProbesTotal: ip(5)}},
	}
	got, err := State(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":"net.swee.washhouse.state.v1","timestamp":"2001-01-01T00:00:00.000Z","stored_kwh":21.79,"usable_l40":263.5,"usable_l40_low":223.975,"low_hot_water":false,"partial":null,"boiler":{"hw":true,"ch":null},"ambient_c":25.7,"active_draw":{"draw_id":"a-1","started_at":"2001-01-01T00:00:00.000Z","last_litre_at":"2001-01-01T00:00:00.000Z","litres":1,"flow_l_min":null,"kind":"tap"},"meter":{"timestamp":"2001-01-01T00:00:00.000Z","counting_since":"2000-12-31T23:59:50.000Z","count":54,"litres_since_start":54,"continuous_flow":{"since":"2000-12-31T23:59:50.000Z","last":"2001-01-01T00:00:00.000Z","litres":2,"flow_l_min":0.1}},"tanktemp":{"timestamp":null,"online":false,"version":null},"tanks":[{"device_id":"hot_water_cylinder_left","state":"heating","heat_source":"hw","stored_kwh":11,"usable_l40":205.8,"top_c":63.5}]}`
	if string(got) != want {
		t.Errorf("state\n got %s\nwant %s", got, want)
	}
	tk, err := Tank(s, s.Tanks[0])
	if err != nil {
		t.Fatal(err)
	}
	want = `{"schema":"net.swee.washhouse.tank.v1","device_id":"hot_water_cylinder_left","timestamp":"2001-01-01T00:00:00.000Z","sweep_at":"2000-12-31T23:59:50.000Z","state":"heating","heat_source":"hw","stored_kwh":11,"mean_c":50,"top_c":63.5,"hot_l":150,"usable_l40":205.8,"usable_l40_low":175,"power_kw":9.8,"ua_w_per_k":4,"probes_ok":5,"probes_total":5,"fault":null}`
	if string(tk) != want {
		t.Errorf("tank\n got %s\nwant %s", tk, want)
	}
}

// A blind tank: the state's partial, the tank's fault, and the two events
// (docs/WIRE.md, Faults).
func TestBlindTankMapping(t *testing.T) {
	since := t0.Add(-5 * time.Minute)
	s := engine.State{At: t0, LowHotWater: bp(false),
		Partial: &engine.Partial{Missing: []string{right}, StoredKWh: 11.275, UsableL40: 307.3111, UsableL40Low: 261.2144},
		Tanks: []engine.TankState{{DeviceID: left, State: thermal.StateCooling, StoredKWh: f(11.275), UsableL40: f(307.3111), TopC: f(63.875)},
			{DeviceID: right, State: thermal.StateUnknown, UAWPerK: f(17), ProbesOK: ip(0), ProbesTotal: ip(5),
				Fault: &engine.Fault{Since: since, Reason: engine.FaultProbes, Errors: map[string]int{"not_present": 5}, Lost: true, LostAt: &t0}}}}
	got, err := State(s)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"low_hot_water":false,"partial":{"missing":["hot_water_cylinder_right"],"stored_kwh":11.275,"usable_l40":307.311,"usable_l40_low":261.214},`; !strings.Contains(string(got), want) {
		t.Errorf("state %s\nwant it to contain %s", got, want)
	}
	tk, err := Tank(s, s.Tanks[1])
	if err != nil {
		t.Fatal(err)
	}
	if want := `"probes_ok":0,"probes_total":5,"fault":{"since":"2000-12-31T23:55:00.000Z","reason":"probes","errors":{"not_present":5},"lost_at":"2001-01-01T00:00:00.000Z"}}`; !strings.HasSuffix(string(tk), want) {
		t.Errorf("tank %s\nwant it to end %s", tk, want)
	}
	for _, c := range []struct {
		in   engine.Event
		want string
	}{
		{engine.Event{Type: engine.EventTankLost, At: t0, Lost: &engine.TankLost{DeviceID: right, Reason: engine.FaultProbes,
			Errors: map[string]int{"bus_low": 5}, Since: since}},
			`{"schema":"net.swee.washhouse.event.v1","event":"tank_lost","timestamp":"2001-01-01T00:00:00.000Z","tank_lost":{"device_id":"hot_water_cylinder_right","reason":"probes","errors":{"bus_low":5},"since":"2000-12-31T23:55:00.000Z"}}`},
		{engine.Event{Type: engine.EventTankRestored, At: t0, Restored: &engine.TankRestored{DeviceID: right, LostSince: since,
			Duration: 4*time.Minute + 30*time.Second}},
			`{"schema":"net.swee.washhouse.event.v1","event":"tank_restored","timestamp":"2001-01-01T00:00:00.000Z","tank_restored":{"device_id":"hot_water_cylinder_right","lost_since":"2000-12-31T23:55:00.000Z","duration_s":270}}`},
	} {
		m, err := Event(c.in, "water/hot")
		if err != nil || string(m.Payload) != c.want {
			t.Errorf("%s: %v\n got %s\nwant %s", c.in.Type, err, m.Payload, c.want)
		}
	}
}

func ip(x int) *int { return &x }

// A source of none is null on the wire, however the engine says it.
func TestNoneSourceIsNull(t *testing.T) {
	none := thermal.SourceNone
	s := state(t0)
	s.Tanks[0].HeatSource = &none
	b, err := Tank(s, s.Tanks[0])
	if err != nil || !strings.Contains(string(b), `"heat_source":null`) {
		t.Errorf("got %s, %v", b, err)
	}
}

// While a sweep is still arriving the state is held, so one goes out per
// sweep (the owner, 24 Nov 2026); a tank's own message still goes. An
// input that made events publishes the state at once: the state that drops
// active_draw follows draw_ended in the same publish (WIRE.md).
func TestStateHeldWhileASweepArrives(t *testing.T) {
	p := newP(t)
	if _, err := p.After(state(t0), nil); err != nil {
		t.Fatal(err)
	}
	s := state(t0.Add(30 * time.Second))
	s.StoredKWh = f(21)
	s.Tanks[0].PowerKW = f(-0.3)
	s.SweepPending = true
	ms, _ := p.After(s, nil)
	if got := topics(ms); got != "test/water/hot/tank/"+left {
		t.Errorf("pending: topics %q, want the left tank's only", got)
	}
	s.SweepPending = false
	ms, _ = p.After(s, nil)
	if got := topics(ms); got != "test/water/hot/state" {
		t.Errorf("the sweep in: topics %q, want the held state", got)
	}
	// With an event, the pending state goes anyway.
	s = state(t0.Add(time.Minute))
	s.StoredKWh = f(19)
	s.SweepPending = true
	ev := engine.Event{Type: engine.EventTankStateChanged, At: s.At, Tank: &engine.TankEvent{DeviceID: left, State: thermal.StateCooling, Prev: thermal.StateUnknown}}
	ms, _ = p.After(s, []engine.Event{ev})
	if got := topics(ms); !strings.HasPrefix(got, "test/water/hot/events ") || !strings.HasSuffix(got, " test/water/hot/state") {
		t.Errorf("pending with an event: topics %q, want the event first and the state last", got)
	}
}
