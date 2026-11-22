package replay

import (
	"strings"
	"testing"
	"time"
)

const sample = `2001-01-01T00:00:00+0000 water/hot/tanktemp/meter {"device_id":"hot_water_meter","timestamp":"2001-01-01T00:00:00Z","clock_synced":true,"trigger":"sweep","seq":1,"litres_per_pulse":1,"level":"open","counting_since":"2000-12-31T23:59:30Z","count":0,"last_pulse_at":null,"last_interval_ms":null,"last_closed_ms":null,"noise":0,"short":0,"glitches":0,"lost":0}
2001-01-01T00:00:05+0000 zigbee2mqtt/climate_groundfloor_boiler-room {"humidity":22,"temperature":18.5}
2001-01-01T00:00:06+0000 energy/boiler/sensor/events {"boiler":{"timestamp":"2001-01-01T00:00:06Z","event":"HW_ON","ch":{"state":"OFF"},"hw":{"state":"ON"}}}
2001-01-01T00:00:07+0000 house/state/devices/hot_water {"id":"hot_water"}
2001-01-01T00:00:08+0000 water/hot/tanktemp/status {"online":false,"event":"LWT"}
`

func TestReadDecodesInOrderAndSkipsOtherTopics(t *testing.T) {
	msgs, err := Read(strings.NewReader(sample), Options{Ambient: "zigbee2mqtt/climate_groundfloor_boiler-room"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("%d messages: %+v", len(msgs), msgs)
	}
	if msgs[0].Meter == nil || msgs[0].Meter.Seq != 1 {
		t.Errorf("meter %+v", msgs[0])
	}
	if a := msgs[1].Ambient; a == nil || a.TempC != 18.5 || !a.At.Equal(time.Date(2001, 1, 1, 0, 0, 5, 0, time.UTC)) {
		t.Errorf("ambient %+v", msgs[1])
	}
	if msgs[2].BoilerEvent == nil || msgs[2].BoilerEvent.Event != "HW_ON" {
		t.Errorf("boiler %+v", msgs[2])
	}
	if msgs[3].Status == nil || msgs[3].Status.Online {
		t.Errorf("status %+v", msgs[3])
	}
	for _, m := range msgs {
		if m.Received.IsZero() || m.Topic == "" {
			t.Errorf("message without receive time or topic: %+v", m)
		}
	}
}

func TestReadRefusesABadLine(t *testing.T) {
	bad := "2001-01-01T00:00:00+0000 water/hot/tanktemp/meter {\"nope\":1}\n"
	if _, err := Read(strings.NewReader(bad), Options{}); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("err %v", err)
	}
}

func TestReadFixture(t *testing.T) {
	msgs, err := ReadFile("../../testdata/fixtures/quiet-night.log.gz", Options{Ambient: "zigbee2mqtt/climate_groundfloor_boiler-room"})
	if err != nil {
		t.Fatal(err)
	}
	n := map[string]int{}
	for _, m := range msgs {
		switch {
		case m.State != nil:
			n["state"]++
		case m.Meter != nil:
			n["meter"]++
		case m.Ambient != nil:
			n["ambient"]++
		case m.BoilerSystem != nil:
			n["system"]++
		}
	}
	if n["state"] != 1870 || n["meter"] != 935 || n["ambient"] != 33 || n["system"] != 34 {
		t.Errorf("counts %v", n)
	}
}
