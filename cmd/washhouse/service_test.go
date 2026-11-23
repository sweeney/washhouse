package main

import (
	"bufio"
	"compress/gzip"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/common/lifecycle"
	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/mqttpubtest"
	"github.com/sweeney/common/wire/washhouse"
	"github.com/sweeney/washhouse/internal/behaviour"
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

func newTestService(t *testing.T, base string) (*service, *mqttpubtest.Fake, *clock) {
	t.Helper()
	f, err := os.Open(behaviourExample)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bc, ec, err := behaviour.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	lc := localConfig{MQTT: mqttConfig{BaseTopic: base, TanktempTopic: "water/hot/tanktemp", BoilerTopic: "energy/boiler/sensor"}}
	fake := &mqttpubtest.Fake{}
	c := &clock{}
	s, err := newService(lc, bc, ec, fake, c.now, log.New(io.Discard, "", 0))
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
	f, err := os.Open(behaviourExample)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bc, ec, err := behaviour.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	lc := localConfig{MQTT: mqttConfig{BaseTopic: "water/hot", TanktempTopic: "water", BoilerTopic: "energy/boiler/sensor"}}
	if _, err := newService(lc, bc, ec, &mqttpubtest.Fake{}, time.Now, nil); err == nil || !strings.Contains(err.Error(), "reads") {
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
		{"silent", true, s.fresh.Silence + time.Second, 0, "degraded", "no input for 2m1s"},
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
// replay) doesn't restart the silence: it isn't news.
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
	c.t = wall.Add(s.fresh.Silence)
	s.tick()
	st, err := washhouse.DecodeState([]byte(fake.Snapshot()[len(fake.Snapshot())-1].Payload))
	if len(fake.Snapshot()) == n || err != nil || !st.Timestamp.Equal(event.Add(s.fresh.Silence)) {
		t.Errorf("no tick at the silence (%d new, %v)", len(fake.Snapshot())-n, err)
	}
}
