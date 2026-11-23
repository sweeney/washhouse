package ingest

import (
	"strings"
	"testing"
	"time"

	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/wire/boiler"
	"github.com/sweeney/common/wire/tanktemp"
)

var live = Sources{
	Tanktemp: []string{"water/hot/tanktemp"},
	Boiler:   "energy/boiler/sensor",
	Ambient:  "zigbee2mqtt/climate_room",
}

var now = time.Date(2001, 1, 1, 7, 0, 0, 0, time.UTC)

// Every topic Decode reads is one a filter subscribes to, and nothing
// else is subscribed: the subscriptions and the decoder can't drift.
func TestFiltersCoverWhatDecodeReads(t *testing.T) {
	read := []string{
		"water/hot/tanktemp/hot_water_cylinder_left/state",
		"water/hot/tanktemp/status",
		"water/hot/tanktemp/meter",
		"energy/boiler/sensor/events",
		"energy/boiler/sensor/system",
		"zigbee2mqtt/climate_room",
	}
	filters := live.Filters()
	for _, topic := range read {
		n := 0
		for _, f := range filters {
			if mqttpub.Match(f, topic) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: matched by %d filters, want 1 (%v)", topic, n, filters)
		}
	}
	if len(filters) != len(read) {
		t.Errorf("%d filters for %d topics: %v", len(filters), len(read), filters)
	}
	for _, f := range filters {
		if err := mqttpub.ValidFilter(f); err != nil {
			t.Error(err)
		}
	}
}

func TestNoAmbientNoAmbientFilter(t *testing.T) {
	s := live
	s.Ambient = ""
	for _, f := range s.Filters() {
		if f == "" || strings.Contains(f, "zigbee") {
			t.Errorf("filter %q", f)
		}
	}
}

// Filters never subscribe to washhouse's own outputs: water/hot/# would
// read back every state it publishes.
func TestFiltersAreNarrow(t *testing.T) {
	for _, f := range live.Filters() {
		if strings.HasSuffix(f, "#") {
			t.Errorf("filter %q", f)
		}
		if mqttpub.Match(f, "water/hot/state") || mqttpub.Match(f, "water/hot/tank/x") {
			t.Errorf("filter %q reads washhouse's own output", f)
		}
	}
}

func TestSourcesValidate(t *testing.T) {
	if err := live.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Sources{
		"no tanktemp":       {Boiler: "b"},
		"empty tanktemp":    {Tanktemp: []string{""}, Boiler: "b"},
		"no boiler":         {Tanktemp: []string{"t"}},
		"wildcard tanktemp": {Tanktemp: []string{"t/+"}, Boiler: "b"},
		"wildcard boiler":   {Tanktemp: []string{"t"}, Boiler: "b/#"},
		"wildcard ambient":  {Tanktemp: []string{"t"}, Boiler: "b", Ambient: "z/+"},
		"trailing slash":    {Tanktemp: []string{"t/"}, Boiler: "b"},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func state(at time.Time, synced bool) Msg {
	return Msg{Received: now, Topic: "water/hot/tanktemp/a/state",
		State: &tanktemp.State{DeviceID: "a", Timestamp: at, ClockSynced: synced}}
}

func TestGate(t *testing.T) {
	g := Gate{Ahead: 90 * time.Second, Stale: 90 * time.Second}
	at := func(d time.Duration) time.Time { return now.Add(d) }
	ts := at(time.Hour)
	for _, c := range []struct {
		name     string
		m        Msg
		retained bool
		ok       bool
	}{
		{"a live sweep", state(at(-time.Second), true), false, true},
		{"a live sweep just ahead", state(at(90*time.Second), true), false, true},
		{"a sweep too far ahead", state(at(90*time.Second+time.Millisecond), true), false, false},
		{"a retained sweep too far ahead", state(at(91*time.Second), true), true, false},
		{"an unsynced sweep ahead (the engine skips it)", state(at(time.Hour), false), false, true},
		{"a retained sweep, current", state(at(-90*time.Second), true), true, true},
		{"a retained sweep, stale", state(at(-91*time.Second), true), true, false},
		{"a live sweep, old (the engine orders it)", state(at(-time.Hour), true), false, true},
		{"a retained sweep, unsynced", state(at(-time.Hour), false), true, true},
		{"a meter ahead", Msg{Meter: &tanktemp.Meter{Timestamp: at(time.Hour), ClockSynced: true}}, false, false},
		{"an unsynced meter ahead", Msg{Meter: &tanktemp.Meter{Timestamp: at(time.Hour)}}, false, true},
		{"a retained meter, old (the baseline)", Msg{Meter: &tanktemp.Meter{Timestamp: at(-time.Hour), ClockSynced: true}}, true, true},
		{"a status ahead", Msg{Status: &tanktemp.Status{Online: true, Timestamp: &ts}}, false, false},
		{"tanktemp's last will", Msg{Status: &tanktemp.Status{}}, true, true},
		{"a boiler event ahead", Msg{BoilerEvent: &boiler.Event{Timestamp: at(time.Hour)}}, false, false},
		{"a retained boiler event, old", Msg{BoilerEvent: &boiler.Event{Timestamp: at(-time.Hour)}}, true, true},
		{"a boiler system ahead", Msg{BoilerSystem: &boiler.System{Timestamp: at(time.Hour)}}, false, false},
		{"the room", Msg{Ambient: &Ambient{At: now, TempC: 20}}, true, true},
	} {
		err := g.Admit(c.m, c.retained, now)
		if (err == nil) != c.ok {
			t.Errorf("%s: admitted %v (%v), want %v", c.name, err == nil, err, c.ok)
		}
	}
}

func TestGateValidate(t *testing.T) {
	if err := (Gate{Ahead: time.Second, Stale: time.Second}).Validate(); err != nil {
		t.Error(err)
	}
	for _, g := range []Gate{{}, {Ahead: time.Second}, {Stale: time.Second}, {Ahead: -1, Stale: 1}} {
		if g.Validate() == nil {
			t.Errorf("%+v: no error", g)
		}
	}
}

func TestDecodeRoutesByTopic(t *testing.T) {
	for _, c := range []struct {
		topic, payload string
		ok             bool
		got            func(Msg) bool
	}{
		{"energy/boiler/sensor/events", `{"boiler":{"timestamp":"2001-01-01T06:30:01Z","event":"HW_ON","ch":{"state":"ON"},"hw":{"state":"ON"}}}`, true, func(m Msg) bool { return m.BoilerEvent != nil }},
		{"zigbee2mqtt/climate_room", `{"temperature":19.5,"humidity":50}`, true, func(m Msg) bool { return m.Ambient != nil && m.Ambient.TempC == 19.5 && m.Ambient.At.Equal(now) }},
		{"zigbee2mqtt/climate_room", `{"humidity":50}`, false, nil},
		{"water/hot/tanktemp/a/b/state", `{}`, false, nil},
		{"water/hot/tanktemp/other", `{}`, false, nil},
		{"water/hot/state", `{}`, false, nil},
	} {
		m, ok, err := live.Decode(c.topic, []byte(c.payload), now)
		if ok != c.ok {
			t.Errorf("%s %s: ok %v (%v), want %v", c.topic, c.payload, ok, err, c.ok)
			continue
		}
		if ok && (err != nil || !c.got(m)) {
			t.Errorf("%s: %+v, %v", c.topic, m, err)
		}
		if ok && (m.Topic != c.topic || !m.Received.Equal(now)) {
			t.Errorf("%s: topic %q received %v", c.topic, m.Topic, m.Received)
		}
	}
}

func TestDecodeErrorIsReported(t *testing.T) {
	_, ok, err := live.Decode("water/hot/tanktemp/meter", []byte(`{`), now)
	if !ok || err == nil {
		t.Errorf("ok %v err %v", ok, err)
	}
}
