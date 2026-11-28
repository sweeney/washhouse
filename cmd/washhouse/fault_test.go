package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/mqttpubtest"
)

// The right tank's bus failing for 46 h (right-bus-lost): its blind sweeps
// are neither feed errors nor a log line each; the loss and the restore are
// one line each; /healthz is degraded while it is lost, naming it, and ok
// once it is restored (sweeney/washhouse#17).
func TestABlindTankOnHealthAndInTheLog(t *testing.T) {
	var logged bytes.Buffer
	lc := localConfig{MQTT: mqttConfig{BaseTopic: "water/hot", TanktempTopic: "water/hot/tanktemp", BoilerTopic: "energy/boiler/sensor"}}
	c := &clock{}
	s, err := newService(lc, exampleDoc(t, nil), &mqttpubtest.Fake{}, c.now, log.New(&logged, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	lost := start.Add(30*time.Minute + 17*time.Second + 5*time.Minute) // since 0:30:17, plus tank_fault_after
	back := start.Add(46*time.Hour + 53*time.Minute + 23*time.Second)
	checked := map[string]bool{}
	for _, l := range readLines(t, "../../testdata/fixtures/right-bus-lost.log.gz") {
		c.t = l.at
		s.handle(mqttpub.Received{Topic: l.topic, Payload: l.payload})
		switch {
		case !checked["before"] && l.at.After(lost.Add(-time.Minute)):
			checked["before"] = true
			if r := s.report(true, c.t); r.Status != "ok" {
				t.Errorf("blind under tank_fault_after: %s %q, want ok", r.Status, r.Detail)
			}
		case !checked["lost"] && l.at.After(lost.Add(time.Minute)):
			checked["lost"] = true
			r := s.report(true, c.t)
			if r.Status != "degraded" || !strings.Contains(r.Detail, "hot_water_cylinder_right") ||
				!strings.Contains(r.Detail, "blind since 2001-01-01T00:30:17Z") || !strings.Contains(r.Detail, "probes") {
				t.Errorf("lost: %s %q, want degraded naming the right tank, since and why", r.Status, r.Detail)
			}
		case !checked["restored"] && l.at.After(back.Add(3*time.Minute)):
			checked["restored"] = true
			if r := s.report(true, c.t); r.Status != "ok" {
				t.Errorf("restored: %s %q, want ok", r.Status, r.Detail)
			}
		}
	}
	if len(checked) != 3 {
		t.Fatalf("checked %v: the fixture ended early", checked)
	}
	if h := s.health(); h.feedErrors != 0 {
		t.Errorf("%d feed errors: a blind tank's sweeps aren't errors", h.feedErrors)
	}
	out := logged.String()
	if n := strings.Count(out, "fewer than two usable probes"); n != 0 {
		t.Errorf("the blind sweeps logged %d times", n)
	}
	if n, m := strings.Count(out, "hot_water_cylinder_right lost"), strings.Count(out, "hot_water_cylinder_right restored"); n != 1 || m != 1 {
		t.Errorf("logged the loss %d times and the restore %d, want once each:\n%s", n, m, out)
	}
}
