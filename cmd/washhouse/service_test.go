package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/common/health"
	"github.com/sweeney/common/lifecycle"
	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/mqttpubtest"
	"github.com/sweeney/common/wire/washhouse"
)

const (
	behaviourExample = "../../testdata/behaviour.example.json"
	showerFixture    = "../../testdata/fixtures/shower-layered.log.gz"
	showerWire       = "../washreplay/testdata/shower-layered.wire"
)

// line is one recorded message, raw, as the broker would hand it over.
type line struct {
	at      time.Time
	topic   string
	payload []byte
}

func readLines(t *testing.T, path string) []line {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var out []line
	sc := bufio.NewScanner(z)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		p := strings.SplitN(sc.Text(), " ", 3)
		at, err := time.Parse("2006-01-02T15:04:05-0700", p[0])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, line{at.UTC(), p[1], []byte(p[2])})
	}
	return out
}

// clock is the wall clock, set by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// exampleDoc is the example behaviour document, edited.
func exampleDoc(t *testing.T, edit func(string) string) document {
	t.Helper()
	b, err := os.ReadFile(behaviourExample)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if edit != nil {
		s = edit(s)
	}
	d, err := parseDocument([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func replace(from, to string) func(string) string {
	return func(s string) string { return strings.Replace(s, from, to, 1) }
}

func newTestService(t *testing.T, base string) (*service, *mqttpubtest.Fake, *clock) {
	t.Helper()
	lc := localConfig{MQTT: mqttConfig{BaseTopic: base, TanktempTopic: "water/hot/tanktemp", BoilerTopic: "energy/boiler/sensor"}}
	fake := &mqttpubtest.Fake{}
	c := &clock{}
	s, err := newService(lc, exampleDoc(t, nil), fake, c.now, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return s, fake, c
}

func wireLines(msgs []mqttpub.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Topic + " " + m.Payload + "\n")
	}
	return b.String()
}

// The service publishes exactly what washreplay -wire does for the same
// recording: the same messages, in the same order, byte for byte. Each
// message arrives when it was recorded; at the end, as washreplay does, a
// silence long enough for a draw still pending to settle.
func TestServicePublishesWhatWashreplayWould(t *testing.T) {
	want, err := os.ReadFile(showerWire)
	if err != nil {
		t.Fatal(err)
	}
	s, fake, c := newTestService(t, "water/hot")
	lines := readLines(t, showerFixture)
	for _, l := range lines {
		c.t = l.at
		s.handle(mqttpub.Received{Topic: l.topic, Payload: l.payload})
	}
	if s.eng.Pending() { // washreplay's last tick: the last input's time plus Settle, if a draw is pending
		c.t = s.lastWall.Add(s.eng.Settle())
		s.tick()
	}
	if got := wireLines(fake.Snapshot()); got != string(want) {
		g, w := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := range min(len(g), len(w)) {
			if g[i] != w[i] {
				t.Fatalf("line %d differs (%d lines, want %d)\n got %s\nwant %s", i+1, len(g), len(w), g[i], w[i])
			}
		}
		t.Fatalf("%d lines, want %d", len(g), len(w))
	}
	for _, m := range fake.Snapshot() {
		retained := !strings.HasSuffix(m.Topic, "/events")
		if m.Retained != retained || (m.QOS == 1) == retained {
			t.Fatalf("%s: retained %v QoS %d", m.Topic, m.Retained, m.QOS)
		}
	}
	h := s.health()
	if h.received != len(lines) || h.refused != 0 || h.decodeErrors != 0 || h.dropped != 0 {
		t.Errorf("health %+v", h)
	}
}

// Feeding the fixture until a draw has started, then nothing: the draw
// ends only once silence has passed, at the time of its last litre, and
// the state is dated by event time moved on by the clock, not the clock.
func TestSilenceMovesEventTimeOn(t *testing.T) {
	s, fake, c := newTestService(t, "water/hot")
	lines := readLines(t, showerFixture)
	var last time.Time
	for _, l := range lines {
		c.t = l.at
		s.handle(mqttpub.Received{Topic: l.topic, Payload: l.payload})
		if st := s.eng.State(); st.ActiveDraw != nil {
			last = st.At
			break
		}
	}
	if last.IsZero() {
		t.Fatal("no draw started")
	}
	arrived := c.t
	silence := s.fresh.Silence

	// The clock runs an hour ahead of event time: the tick must not use it.
	offset := time.Hour
	c.t = arrived.Add(offset + silence - time.Second)
	s.lastWall = arrived.Add(offset) // as if it arrived on this clock
	s.lastTanktemp = s.lastWall
	n := len(fake.Snapshot())
	s.tick()
	if len(fake.Snapshot()) != n {
		t.Fatalf("published before the silence: %s", wireLines(fake.Snapshot()[n:]))
	}
	c.t = arrived.Add(offset + silence + s.eng.Settle())
	s.tick()
	var ended bool
	for _, m := range fake.Snapshot()[n:] {
		if m.Topic == "water/hot/events" && strings.Contains(m.Payload, `"draw_ended"`) {
			ended = true
		}
		if m.Topic == "water/hot/state" {
			st, err := washhouse.DecodeState([]byte(m.Payload))
			if err != nil {
				t.Fatal(err)
			}
			if want := last.Add(silence + s.eng.Settle()); !st.Timestamp.Equal(want) {
				t.Errorf("state dated %v, want %v (event time moved on)", st.Timestamp.Time, want)
			}
			if st.ActiveDraw != nil {
				t.Error("the draw is still active")
			}
		}
	}
	if !ended {
		t.Errorf("no draw_ended: %s", wireLines(fake.Snapshot()[n:]))
	}
}

func TestTickBeforeAnyInputDoesNothing(t *testing.T) {
	s, fake, c := newTestService(t, "water/hot")
	c.t = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	s.tick()
	if n := len(fake.Snapshot()); n != 0 {
		t.Errorf("%d published", n)
	}
}

// A sweep dated far past the clock is refused and counted, and doesn't
// move event time: the next real sweep is still taken.
func TestFutureSweepIsRefused(t *testing.T) {
	s, fake, c := newTestService(t, "water/hot")
	lines := readLines(t, showerFixture)
	var sweep line
	for _, l := range lines {
		if strings.HasSuffix(l.topic, "/state") {
			sweep = l
			break
		}
	}
	c.t = sweep.at.Add(-time.Hour) // the sweep is an hour ahead of the clock
	s.handle(mqttpub.Received{Topic: sweep.topic, Payload: sweep.payload})
	if h := s.health(); h.refused != 1 {
		t.Errorf("refused %d", h.refused)
	}
	if n := len(fake.Snapshot()); n != 0 {
		t.Errorf("%d published", n)
	}
	if at := s.eng.State().At; !at.IsZero() {
		t.Errorf("event time %v", at)
	}
}

func TestUndecodableIsCountedNotFatal(t *testing.T) {
	s, _, c := newTestService(t, "water/hot")
	c.t = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	s.handle(mqttpub.Received{Topic: "water/hot/tanktemp/meter", Payload: []byte(`{`)})
	s.handle(mqttpub.Received{Topic: "somewhere/else", Payload: []byte(`{`)})
	if h := s.health(); h.received != 2 || h.decodeErrors != 1 {
		t.Errorf("health %+v", h)
	}
}

// A handler never blocks paho: a full queue drops and counts.
func TestFullQueueDrops(t *testing.T) {
	s, _, _ := newTestService(t, "water/hot")
	s.in = make(chan mqttpub.Received, 1)
	s.enqueue(mqttpub.Received{Topic: "a"})
	s.enqueue(mqttpub.Received{Topic: "b"})
	if h := s.health(); h.dropped != 1 {
		t.Errorf("dropped %d", h.dropped)
	}
}

func TestEnqueueKeepsTheMessage(t *testing.T) {
	s, _, _ := newTestService(t, "water/hot")
	s.in = make(chan mqttpub.Received, 1)
	s.enqueue(mqttpub.Received{Topic: "a", Payload: []byte("x"), Retained: true})
	if r := <-s.in; r.Topic != "a" || string(r.Payload) != "x" || !r.Retained {
		t.Errorf("%+v", r)
	}
}

func TestSubscribesToTheSources(t *testing.T) {
	s, fake, _ := newTestService(t, "water/hot")
	if err := s.subscribe(fake); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.Subscriptions(), " ")
	want := "water/hot/tanktemp/+/state water/hot/tanktemp/status water/hot/tanktemp/meter " +
		"energy/boiler/sensor/events energy/boiler/sensor/system zigbee2mqtt/climate_groundfloor_boiler-room"
	if got != want {
		t.Errorf("subscriptions\n got %s\nwant %s", got, want)
	}
	q := qosRecorder{}
	if err := s.subscribe(q); err != nil {
		t.Fatal(err)
	}
	for f, qos := range q {
		if qos != 1 {
			t.Errorf("%s at QoS %d, want 1 (the meter and the events are published at 1)", f, qos)
		}
	}
}

// qosRecorder records each subscription's QoS.
type qosRecorder map[string]byte

func (q qosRecorder) Subscribe(filter string, qos byte, _ func(mqttpub.Received), _ func()) error {
	q[filter] = qos
	return nil
}

// A subscription that would read washhouse's own outputs back is
// refused: a tanktemp prefix of water subscribes to water/+/state, which
// is water/hot/state.
func TestBaseThatReadsItselfIsRefused(t *testing.T) {
	lc := localConfig{MQTT: mqttConfig{BaseTopic: "water/hot", TanktempTopic: "water", BoilerTopic: "energy/boiler/sensor"}}
	if _, err := newService(lc, exampleDoc(t, nil), &mqttpubtest.Fake{}, time.Now, nil); err == nil || !strings.Contains(err.Error(), "reads") {
		t.Errorf("got %v", err)
	}
}

func TestStatusPayloads(t *testing.T) {
	s, _, c := newTestService(t, "water/hot")
	start := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	s.start, s.version = start, "abc1234"
	c.t = start.Add(90 * time.Second)
	for _, tc := range []struct {
		e     lifecycle.Event
		ready bool
		want  string
	}{
		{lifecycle.Event{Kind: lifecycle.Startup, At: start}, false,
			`{"schema":"net.swee.washhouse.status.v1","online":true,"event":"STARTUP","timestamp":"2001-01-01T00:00:00.000Z","version":"abc1234","ready":false,"uptime_s":0,"tanks":["hot_water_cylinder_left","hot_water_cylinder_right"]}`},
		{lifecycle.Event{Kind: lifecycle.Ready, At: start.Add(90 * time.Second)}, true,
			`{"schema":"net.swee.washhouse.status.v1","online":true,"event":"HEARTBEAT","timestamp":"2001-01-01T00:01:30.000Z","version":"abc1234","ready":true,"uptime_s":90,"tanks":["hot_water_cylinder_left","hot_water_cylinder_right"]}`},
		{lifecycle.Event{Kind: lifecycle.Shutdown, At: start.Add(90 * time.Second), Reason: "SIGTERM"}, true,
			`{"schema":"net.swee.washhouse.status.v1","online":false,"event":"SHUTDOWN","timestamp":"2001-01-01T00:01:30.000Z","version":"abc1234","ready":true,"uptime_s":90,"tanks":["hot_water_cylinder_left","hot_water_cylinder_right"],"reason":"SIGTERM"}`},
	} {
		s.ready.Store(tc.ready)
		m, err := s.status(tc.e)
		if err != nil {
			t.Fatal(err)
		}
		if m.Topic != "water/hot/status" || !m.Retained || m.QOS != 1 || m.Payload != tc.want {
			t.Errorf("%v: %+v\nwant %s", tc.e.Kind, m, tc.want)
		}
	}
}

func TestHealthReport(t *testing.T) {
	s, _, c := newTestService(t, "water/hot")
	t0 := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if r := s.report(true, t0); r.Status != "starting" {
		t.Errorf("before any input: %s %s", r.Status, r.Detail)
	}
	c.t = t0
	s.handle(mqttpub.Received{Topic: "somewhere", Payload: []byte("x")})
	if r := s.report(true, t0); r.Status != "degraded" || r.Detail != "no tanktemp input yet" {
		t.Errorf("inputs but none from tanktemp: %s %q", r.Status, r.Detail)
	}
	s.mu.Lock()
	s.h.lastTanktemp = t0
	s.mu.Unlock()
	for _, tc := range []struct {
		name      string
		connected bool
		at        time.Duration
		dropped   int
		status    string
		detail    string
	}{
		{"working", true, time.Second, 0, "ok", ""},
		{"just within the silence", true, s.fresh.Silence, 0, "ok", ""},
		{"disconnected", false, time.Second, 0, "degraded", "MQTT not connected"},
		{"silent", true, s.fresh.Silence + time.Second, 0, "degraded", "no tanktemp input for 2m1s"},
		{"dropping", true, time.Second, 3, "degraded", "3 inputs dropped"},
		{"dropped one", true, time.Second, 1, "degraded", "1 inputs dropped"},
		{"disconnected and dropping", false, time.Second, 3, "degraded", "MQTT not connected; 3 inputs dropped"},
	} {
		s.mu.Lock()
		s.h.dropped = tc.dropped
		s.mu.Unlock()
		r := s.report(tc.connected, t0.Add(tc.at))
		if r.Status != tc.status || r.Detail != tc.detail {
			t.Errorf("%s: %s %q, want %s %q", tc.name, r.Status, r.Detail, tc.status, tc.detail)
		}
	}
}

// An input that doesn't move event time on (an old programmer event, a
// replay) moves neither the base the silence extrapolates from nor the
// silence itself.
func TestOldInputDoesNotRestartTheSilence(t *testing.T) {
	s, fake, c := newTestService(t, "water/hot")
	lines := readLines(t, showerFixture)
	for _, l := range lines[:200] {
		c.t = l.at
		s.handle(mqttpub.Received{Topic: l.topic, Payload: l.payload})
	}
	wall, event := s.lastWall, s.lastEvent
	c.t = wall.Add(s.fresh.Silence - time.Second)
	s.handle(mqttpub.Received{Topic: "energy/boiler/sensor/events",
		Payload: []byte(`{"boiler":{"timestamp":"2000-12-31T00:00:00Z","event":"HW_ON","ch":{"state":"OFF"},"hw":{"state":"ON"}}}`)})
	if s.lastWall != wall || s.lastEvent != event {
		t.Fatalf("an old event moved the silence: wall %v→%v", wall, s.lastWall)
	}
	n := len(fake.Snapshot())
	c.t = s.lastTanktemp.Add(s.fresh.Silence)
	s.tick()
	st, err := washhouse.DecodeState([]byte(fake.Snapshot()[len(fake.Snapshot())-1].Payload))
	if len(fake.Snapshot()) == n || err != nil || !st.Timestamp.Equal(event.Add(c.t.Sub(wall))) {
		t.Errorf("no tick at the silence (%d new, %v)", len(fake.Snapshot())-n, err)
	}
}

// tanktemp's probe (PR #5 review): the room keeps reporting after
// tanktemp stops. Its readings move event time on but end no draw, so the
// silence that matters is tanktemp's: the draw must still end.
func TestRoomReadingsDoNotHideTanktempsSilence(t *testing.T) {
	s, fake, c := newTestService(t, "water/hot")
	for _, l := range readLines(t, showerFixture) {
		c.t = l.at
		s.handle(mqttpub.Received{Topic: l.topic, Payload: l.payload})
		if s.eng.State().ActiveDraw != nil {
			break
		}
	}
	n := len(fake.Snapshot())
	stopped := c.t
	for at := 90 * time.Second; at <= time.Hour; at += 90 * time.Second {
		c.t = stopped.Add(at)
		s.handle(mqttpub.Received{Topic: "zigbee2mqtt/climate_groundfloor_boiler-room", Payload: []byte(`{"temperature":20}`)})
		s.tick()
	}
	var ended bool
	for _, m := range fake.Snapshot()[n:] {
		ended = ended || (m.Topic == "water/hot/events" && strings.Contains(m.Payload, `"draw_ended"`))
	}
	if !ended || s.eng.State().ActiveDraw != nil {
		t.Errorf("draw_ended %v, active %v", ended, s.eng.State().ActiveDraw != nil)
	}
	if r := s.report(true, c.t); r.Status != "degraded" || !strings.Contains(r.Detail, "no tanktemp input") {
		t.Errorf("health %s %q", r.Status, r.Detail)
	}
}

// Sweeps whose clock isn't synced are skipped by the engine, so they
// don't count as tanktemp being heard: the draw still ends.
func TestUnsyncedSweepsDoNotHideTanktempsSilence(t *testing.T) {
	s, fake, c := newTestService(t, "water/hot")
	lines := readLines(t, showerFixture)
	i := 0
	for ; i < len(lines); i++ {
		c.t = lines[i].at
		s.handle(mqttpub.Received{Topic: lines[i].topic, Payload: lines[i].payload})
		if s.eng.State().ActiveDraw != nil {
			break
		}
	}
	n := len(fake.Snapshot())
	stopped := c.t
	for at := 30 * time.Second; at <= 10*time.Minute; at += 30 * time.Second {
		c.t = stopped.Add(at)
		for _, l := range lines[i+1:] {
			if strings.HasSuffix(l.topic, "/state") {
				p := strings.Replace(string(l.payload), `"clock_synced":true`, `"clock_synced":false`, 1)
				s.handle(mqttpub.Received{Topic: l.topic, Payload: []byte(p)})
				break
			}
		}
		s.tick()
	}
	var ended bool
	for _, m := range fake.Snapshot()[n:] {
		ended = ended || (m.Topic == "water/hot/events" && strings.Contains(m.Payload, `"draw_ended"`))
	}
	if !ended {
		t.Error("no draw_ended")
	}
}

// With tanktemp never heard, time still moves on: the room's reading goes
// stale.
func TestTanktempNeverHeardStillTicks(t *testing.T) {
	s, fake, c := newTestService(t, "water/hot")
	c.t = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	s.handle(mqttpub.Received{Topic: "zigbee2mqtt/climate_groundfloor_boiler-room", Payload: []byte(`{"temperature":20}`)})
	c.t = c.t.Add(time.Hour)
	s.tick()
	m, ok := fake.Find("water/hot/state")
	for _, x := range fake.Snapshot() {
		if x.Topic == "water/hot/state" {
			m = x
		}
	}
	if !ok || !strings.Contains(m.Payload, `"ambient_c":null`) || !strings.Contains(m.Payload, `"timestamp":"2001-01-01T01:00:00.000Z"`) {
		t.Errorf("last state %s", m.Payload)
	}
}

// tanktemp's meter is optional: synced sweeps alone are tanktemp heard
// (tanktemp's re-review of #5).
func TestSweepsAloneAreTanktempHeard(t *testing.T) {
	s, fake, c := newTestService(t, "water/hot")
	var last time.Time
	for _, l := range readLines(t, showerFixture)[:300] {
		if strings.HasSuffix(l.topic, "/state") {
			c.t, last = l.at, l.at
			s.handle(mqttpub.Received{Topic: l.topic, Payload: l.payload})
		}
	}
	c.t = last.Add(time.Second)
	if r := s.report(true, c.t); r.Status != "ok" {
		t.Errorf("health %s %q", r.Status, r.Detail)
	}
	n := len(fake.Snapshot())
	c.t = last.Add(s.fresh.Silence - time.Second)
	s.tick()
	if len(fake.Snapshot()) != n {
		t.Errorf("ticked within the silence: %s", wireLines(fake.Snapshot()[n:]))
	}
}

// A reload of the same document (reformatted is the same) changes
// nothing: the engine, and what it has learnt, carry on.
func TestReloadUnchangedKeepsTheEngine(t *testing.T) {
	s, _, _ := newTestService(t, "water/hot")
	eng := s.eng
	d := exampleDoc(t, replace(`"recent_draws": 50,`, `"recent_draws":   50,`))
	changed, err := s.apply(d)
	if err != nil || changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if s.eng != eng {
		t.Error("the engine was rebuilt")
	}
}

// A changed document rebuilds the engine (a restart, for consumers) and
// the gate, and keeps event time.
func TestReloadChangedRebuilds(t *testing.T) {
	s, fake, c := newTestService(t, "water/hot")
	c.t = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, l := range readLines(t, showerFixture)[:20] {
		c.t = l.at
		s.handle(mqttpub.Received{Topic: l.topic, Payload: l.payload})
	}
	eng, lastEvent, published := s.eng, s.lastEvent, len(fake.Snapshot())
	if lastEvent.IsZero() {
		t.Fatal("no event time")
	}
	d := exampleDoc(t, replace(`"freshness": {"ahead": "90s", "stale": "90s", "silence": "2m"}`,
		`"freshness": {"ahead": "60s", "stale": "90s", "silence": "5m"}`))
	changed, err := s.apply(d)
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if s.eng == eng || s.gate.Ahead != time.Minute || s.silence() != 5*time.Minute {
		t.Errorf("engine rebuilt %v, gate %+v, silence %v", s.eng != eng, s.gate, s.silence())
	}
	s.mu.Lock()
	s.h.lastReceived, s.h.lastTanktemp = c.t, c.t
	s.mu.Unlock()
	if r := s.report(true, c.t.Add(3*time.Minute)); r.Status != "ok" {
		t.Errorf("/healthz judges the silence by the old document: %s %q", r.Status, r.Detail)
	}
	if s.lastEvent != lastEvent {
		t.Errorf("event time %v, was %v", s.lastEvent, lastEvent)
	}
	if n := len(fake.Snapshot()); n != published {
		t.Errorf("a reload published %d messages; the next input publishes", n-published)
	}
	if !bytesEqual(s.raw, d.raw) {
		t.Error("the running document isn't the new one")
	}
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

// The subscriptions are made at start, so a changed room sensor needs a
// restart: the document is refused whole, and nothing changes.
func TestReloadRefusesAnAmbientChange(t *testing.T) {
	s, _, _ := newTestService(t, "water/hot")
	eng, raw := s.eng, s.raw
	d := exampleDoc(t, replace(`"ambient_topic": "zigbee2mqtt/climate_groundfloor_boiler-room"`, `"ambient_topic": "zigbee2mqtt/elsewhere"`))
	changed, err := s.apply(d)
	if err == nil || !strings.Contains(err.Error(), "restart") || changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if s.eng != eng || !bytesEqual(s.raw, raw) {
		t.Error("a refused document changed something")
	}
}

// The status names the tanks of the running document.
func TestReloadRenamesTheTanks(t *testing.T) {
	s, _, _ := newTestService(t, "water/hot")
	d := exampleDoc(t, func(x string) string {
		return strings.ReplaceAll(x, "hot_water_cylinder_left", "cylinder_a")
	})
	if _, err := s.apply(d); err != nil {
		t.Fatal(err)
	}
	m, err := s.status(lifecycle.Event{Kind: lifecycle.Heartbeat, At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Payload, `"tanks":["cylinder_a","hot_water_cylinder_right"]`) {
		t.Errorf("status %s", m.Payload)
	}
}

// reload hands the document to the loop and waits for its answer.
func TestReloadRunsOnTheLoop(t *testing.T) {
	s, _, _ := newTestService(t, "water/hot")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.loop(ctx, nil) }()
	changed, err := s.reload(ctx, exampleDoc(t, replace(`"recent_draws": 50,`, `"recent_draws": 40,`)))
	if err != nil || !changed {
		t.Errorf("changed %v, err %v", changed, err)
	}
	cancel()
	<-done
	if _, err := s.reload(ctx, exampleDoc(t, nil)); err == nil {
		t.Error("a reload with the loop gone didn't fail")
	}
}

// /healthz carries the namespace's status, and is degraded while the
// latest fetch was refused (washhouse runs the last good document).
func TestHealthShowsTheNamespace(t *testing.T) {
	s, _, _ := newTestService(t, "water/hot")
	t0 := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	s.mu.Lock()
	s.h.lastReceived, s.h.lastTanktemp = t0, t0
	s.mu.Unlock()
	s.remote = &remote{ns: "washhouse"}
	s.remote.tracker.Record("washhouse", nil)
	if r := s.report(true, t0); r.Status != "ok" || !hasField(r, "remote_config") {
		t.Errorf("landed: %s %q %v", r.Status, r.Detail, r.Fields)
	}
	s.remote.tracker.Record("washhouse", errors.New("washhouse: config: colour"))
	r := s.report(true, t0)
	if r.Status != "degraded" || r.Detail != "the last behaviour fetch failed or was refused (running the last good): washhouse: config: colour" {
		t.Errorf("refused: %s %q", r.Status, r.Detail)
	}
	s.remote.tracker.Record("washhouse", nil)
	if r := s.report(true, t0); r.Status != "ok" {
		t.Errorf("landed again: %s %q", r.Status, r.Detail)
	}
}

func hasField(r health.Report, key string) bool {
	for _, f := range r.Fields {
		if f.Key == key {
			return true
		}
	}
	return false
}

// A document that fetched and parsed but that the service refuses (here,
// a new room sensor) counts as refused on /healthz.
func TestRefusedReloadShowsOnHealth(t *testing.T) {
	s, _, _ := newTestService(t, "water/hot")
	cs := newConfigService(t)
	r, err := newRemote(localFor(t, cs), "dev")
	if err != nil {
		t.Fatal(err)
	}
	s.remote = r
	b, _ := os.ReadFile(behaviourExample)
	cs.set(strings.Replace(string(b), "climate_groundfloor_boiler-room", "elsewhere", 1), http.StatusOK)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.loop(ctx, nil)
	s.reloadFrom(ctx, r)
	if st := r.status(); st.OK || !strings.Contains(st.Error, "restart") {
		t.Errorf("status %+v", st)
	}
}

// Before any fetch there is nothing refused.
func TestHealthBeforeAnyFetch(t *testing.T) {
	s, _, _ := newTestService(t, "water/hot")
	t0 := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	s.mu.Lock()
	s.h.lastReceived, s.h.lastTanktemp = t0, t0
	s.mu.Unlock()
	s.remote = &remote{ns: "washhouse"}
	if r := s.report(true, t0); r.Status != "ok" {
		t.Errorf("%s %q", r.Status, r.Detail)
	}
}

// /healthz shows each namespace's fetch (when, how long ago, the error),
// and when the running document was applied: an engine rebuilt by a reload
// shows there (statehouse's review of #6).
func TestHealthShowsTheFetchAndTheApply(t *testing.T) {
	s, _, c := newTestService(t, "water/hot")
	now := time.Now()
	c.t = now.Add(-time.Hour)
	if _, err := s.apply(exampleDoc(t, replace(`"recent_draws": 50,`, `"recent_draws": 40,`))); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.h.lastReceived, s.h.lastTanktemp = now, now
	s.mu.Unlock()
	s.remote = &remote{ns: "washhouse"}
	s.remote.tracker.Record("washhouse", errors.New("no"))
	r := s.report(true, now.Add(90*time.Second))
	var ns map[string]namespaceStatus
	var applied *string
	for _, f := range r.Fields {
		switch f.Key {
		case "remote_config":
			ns, _ = f.Value.(map[string]namespaceStatus)
		case "behaviour_applied_at":
			applied, _ = f.Value.(*string)
		}
	}
	st, ok := ns["washhouse"]
	if !ok || st.OK || st.Error != "no" || st.FetchedAt.IsZero() || st.FetchedAgo < 89 || st.FetchedAgo > 91 {
		t.Errorf("remote_config %+v", ns)
	}
	if want := c.t.UTC().Format(time.RFC3339); applied == nil || *applied != want {
		t.Errorf("behaviour_applied_at %v, want %s", applied, want)
	}
}

// A changed reload replays the latest admitted input on each topic into
// the new engine, as a restart's retained messages would, so the
// programmer, the room, tanktemp's status, the tanks and the meter are
// known at once, not when each next speaks (tanktemp's review of #6).
// The replay publishes nothing.
func TestReloadReplaysTheLatestInputs(t *testing.T) {
	s, fake, c := newTestService(t, "water/hot")
	for _, l := range readLines(t, showerFixture) {
		c.t = l.at
		s.handle(mqttpub.Received{Topic: l.topic, Payload: l.payload})
	}
	before, published := s.eng.State(), len(fake.Snapshot())
	if _, err := s.apply(exampleDoc(t, replace(`"recent_draws": 50,`, `"recent_draws": 40,`))); err != nil {
		t.Fatal(err)
	}
	after := s.eng.State()
	same := func(what string, a, b *bool) {
		if a == nil || b == nil || *a != *b {
			t.Errorf("%s: %v, was %v", what, b, a)
		}
	}
	same("low hot water", before.LowHotWater, after.LowHotWater)
	same("boiler hw", before.Boiler.HW, after.Boiler.HW)
	same("boiler ch", before.Boiler.CH, after.Boiler.CH)
	same("tanktemp online", before.Tanktemp.Online, after.Tanktemp.Online)
	if after.AmbientC == nil || *after.AmbientC != *before.AmbientC {
		t.Errorf("ambient %v, was %v", after.AmbientC, before.AmbientC)
	}
	for i, tk := range after.Tanks {
		if tk.StoredKWh == nil || *tk.StoredKWh != *before.Tanks[i].StoredKWh {
			t.Errorf("%s stored %v, was %v", tk.DeviceID, tk.StoredKWh, before.Tanks[i].StoredKWh)
		}
	}
	if after.Meter.Count == nil || *after.Meter.Count != *before.Meter.Count || after.ActiveDraw != nil {
		t.Errorf("meter %+v, active %v", after.Meter, after.ActiveDraw)
	}
	if !after.At.Equal(before.At) {
		t.Errorf("event time %v, was %v", after.At, before.At)
	}
	if n := len(fake.Snapshot()); n != published {
		t.Errorf("the replay published %d messages", n-published)
	}
}

// The latest input on a topic is replayed, whatever the engine made of it:
// a sweep with no usable probe is the tank's present (no estimate), as
// the retained message would be after a restart, not the good sweep
// before it.
func TestReloadReplaysAFailedSweep(t *testing.T) {
	s, _, c := newTestService(t, "water/hot")
	var last line
	for _, l := range readLines(t, showerFixture) {
		c.t = l.at
		s.handle(mqttpub.Received{Topic: l.topic, Payload: l.payload})
		if strings.HasSuffix(l.topic, "cylinder_left/state") {
			last = l
		}
	}
	bad := regexp.MustCompile(`"(temp|raw)_c":[-0-9.]+`).ReplaceAllString(string(last.payload), `"${1}_c":null`)
	bad = strings.ReplaceAll(bad, `"error":null`, `"error":"crc"`)
	at := last.at.Add(30 * time.Second)
	bad = regexp.MustCompile(`"timestamp":"[^"]*"`).ReplaceAllString(bad, `"timestamp":"`+at.UTC().Format(time.RFC3339)+`"`)
	c.t = at
	s.handle(mqttpub.Received{Topic: last.topic, Payload: []byte(bad)})
	before := s.eng.State().Tanks[0]
	if before.StoredKWh != nil {
		t.Fatalf("the bad sweep left an estimate (%+v)", s.health())
	}
	if _, err := s.apply(exampleDoc(t, replace(`"recent_draws": 50,`, `"recent_draws": 40,`))); err != nil {
		t.Fatal(err)
	}
	after := s.eng.State().Tanks[0]
	if after.StoredKWh != nil || before.StoredKWh != nil || after.ProbesOK == nil || *after.ProbesOK != *before.ProbesOK {
		t.Errorf("after the reload %+v\nbefore %+v", after, before)
	}
}
