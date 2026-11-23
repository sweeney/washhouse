package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/mqttpubtest"
	"github.com/sweeney/common/wire/washhouse"
)

func TestVersionFlagPrintsVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := realMain([]string{"-version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "washhouse "+version {
		t.Errorf("got %q", got)
	}
}

func TestUnknownFlagIsAnError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := realMain([]string{"-nope"}, &out, &errOut); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}

func TestMissingConfigIsAnError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := realMain([]string{"-config", filepath.Join(t.TempDir(), "nope.yaml")}, &out, &errOut); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

// writeConfig writes a local config whose behaviour_file is the example.
func writeConfig(t *testing.T, edit func(string) string) string {
	t.Helper()
	b, err := os.ReadFile(exampleLocal)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(behaviourExample)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "/etc/washhouse/behaviour.json", abs, 1)
	if edit != nil {
		s = edit(s)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := realMain([]string{"-config", writeConfig(t, nil), "-check-config"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	for _, want := range []string{": ok: 2 tanks (hot_water_cylinder_left, hot_water_cylinder_right), meter hot_water_meter",
		"reading water/hot/tanktemp and energy/boiler/sensor", "publishing to water/hot on tcp://broker.example:1883"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("%q isn't in %q", want, out.String())
		}
	}
}

func TestCheckConfigRefusesABadBehaviourFile(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "behaviour.json")
	if err := os.WriteFile(bad, []byte(`{"colour":"blue"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := writeConfig(t, func(s string) string {
		abs, _ := filepath.Abs(behaviourExample)
		return strings.Replace(s, abs, bad, 1)
	})
	var out, errOut bytes.Buffer
	if code := realMain([]string{"-config", p, "-check-config"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "colour") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}

// conn is the fake broker connection: mqttpubtest.Fake, plus Connect and
// Reconnected, refusing a Subscribe after Connect as MQTTPublisher does.
type conn struct {
	*mqttpubtest.Fake
	mu          sync.Mutex
	connected   bool
	reconnected chan struct{}
}

func (c *conn) Connect(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	return nil
}

func (c *conn) Subscribe(filter string, qos byte, onMsg func(mqttpub.Received), onLost func()) error {
	c.mu.Lock()
	connected := c.connected
	c.mu.Unlock()
	if connected {
		return errors.New("Subscribe after Connect")
	}
	return c.Fake.Subscribe(filter, qos, onMsg, onLost)
}

func (c *conn) Reconnected() <-chan struct{} { return c.reconnected }

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func statuses(f *mqttpubtest.Fake) []washhouse.Status {
	var out []washhouse.Status
	for _, m := range f.All("water/hot/status") {
		s, err := washhouse.DecodeStatus([]byte(m.Payload))
		if err == nil {
			out = append(out, s)
		}
	}
	return out
}

// run end to end with every outside thing faked: STARTUP, ready once
// connected, inputs published, a HEARTBEAT on reconnect, /healthz, and
// SHUTDOWN last on a signal.
func TestRun(t *testing.T) {
	s, _, _ := newTestService(t, "water/hot")
	c := &conn{Fake: &mqttpubtest.Fake{}, reconnected: make(chan struct{}, 1)}
	s.out = c
	s.wall = time.Now
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sig := make(chan os.Signal, 1)
	tick := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), s, deps{Conn: c, Wall: time.Now, Listener: ln, Signals: sig, Tick: tick,
			HeartbeatTick: make(chan time.Time), Logger: log.New(io.Discard, "", 0), Version: "abc1234"})
	}()

	waitFor(t, "STARTUP", func() bool { return len(statuses(c.Fake)) >= 1 })
	if st := statuses(c.Fake)[0]; st.Event != washhouse.StatusStartup || st.Version != "abc1234" {
		t.Errorf("first status %+v", st)
	}
	if len(c.Subscriptions()) != 6 {
		t.Errorf("subscriptions %v", c.Subscriptions())
	}
	tick <- time.Now() // the first unit of work is immediate; this is the second
	waitFor(t, "the ready HEARTBEAT", func() bool {
		ss := statuses(c.Fake)
		return len(ss) >= 2 && ss[len(ss)-1].Event == washhouse.StatusHeartbeat && ss[len(ss)-1].Ready
	})

	// A boiler heartbeat, delivered as paho would, dated now.
	now := time.Now().UTC().Format(time.RFC3339)
	c.Deliver("energy/boiler/sensor/system", []byte(`{"status":{"event":"HEARTBEAT","ch":"OFF","hw":"ON","ready":true,"timestamp":"`+now+`"}}`), true)
	waitFor(t, "the state", func() bool { _, ok := c.Find("water/hot/state"); return ok })
	if m, _ := c.Find("water/hot/state"); !strings.Contains(m.Payload, `"boiler":{"hw":true,"ch":false}`) {
		t.Errorf("state %s", m.Payload)
	}

	n := len(statuses(c.Fake))
	c.reconnected <- struct{}{}
	waitFor(t, "the reconnect HEARTBEAT", func() bool { return len(statuses(c.Fake)) > n })

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`"status": "ok"`, `"version": "abc1234"`, `"mqtt_connected": true`, `"ready": true`, `"received": 1`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("%s isn't in /healthz:\n%s", want, body)
		}
	}

	sig <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run didn't return")
	}
	ss := statuses(c.Fake)
	last := ss[len(ss)-1]
	if last.Event != washhouse.StatusShutdown || last.Online || last.Reason == nil || *last.Reason != "SIGTERM" {
		t.Errorf("last status %+v", last)
	}
	if !c.Closed {
		t.Error("the connection wasn't closed")
	}
}
