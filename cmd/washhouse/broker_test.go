package main

import (
	"context"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/mqtttest"
	"github.com/sweeney/common/wire/washhouse"
)

// fastTuning keeps a real connection's timeouts short for a test.
func fastTuning() mqttpub.Tuning {
	tn := mqttpub.DefaultTuning()
	tn.PublishTimeout = 2 * time.Second
	return tn
}

// Against a real Mosquitto: the programmer's retained heartbeat, published
// before washhouse starts, reaches it on subscribing; washhouse's state is
// retained on the broker with it; a clean stop leaves SHUTDOWN retained,
// and an unclean one the last will.
func TestAgainstARealBroker(t *testing.T) {
	b := mqtttest.StartBroker(t, "")
	feeder, err := mqttpub.NewMQTTPublisherWith(mqttpub.Options{Broker: b.URL(), ClientID: "feeder", WillQOS: 1},
		"feeder/status", `{"online":false}`, fastTuning())
	if err != nil {
		t.Fatal(err)
	}
	if err := feeder.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer feeder.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := feeder.Publish(mqttpub.Message{Topic: "energy/boiler/sensor/system", Retained: true, QOS: 1,
		Payload: `{"status":{"event":"HEARTBEAT","ch":"OFF","hw":"ON","ready":true,"timestamp":"` + now + `"}}`}); err != nil {
		t.Fatal(err)
	}

	start := func(url, id string) (chan os.Signal, chan error) {
		s, _, _ := newTestService(t, "water/hot")
		pub, err := mqttpub.NewMQTTPublisherWith(mqttpub.Options{Broker: url, ClientID: id, WillQOS: 1},
			washhouse.StatusTopic("water/hot"), string(washhouse.LWT()), fastTuning())
		if err != nil {
			t.Fatal(err)
		}
		s.out, s.wall = pub, time.Now
		sig := make(chan os.Signal, 1)
		done := make(chan error, 1)
		go func() {
			done <- run(context.Background(), s, deps{Conn: pub, Wall: time.Now, Signals: sig, Heartbeat: time.Hour,
				Logger: log.New(io.Discard, "", 0), Version: "test", Broker: url, ClientID: id})
		}()
		return sig, done
	}
	retained := func(topic, want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			got, ok := mqtttest.Retained(t, b.URL(), topic)
			if ok && strings.Contains(got, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: retained %q (%v), want it to contain %s", topic, got, ok, want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	sig, done := start(b.URL(), "washhouse-1")
	retained("water/hot/state", `"boiler":{"hw":true,"ch":false}`)
	retained("water/hot/status", `"ready":true`)
	sig <- syscall.SIGTERM
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	retained("water/hot/status", `"event":"SHUTDOWN"`)

	// An unclean end, the connection cut: the broker publishes the will.
	p := mqtttest.StartProxy(t, b.Addr)
	sig, done = start(p.URL(), "washhouse-2")
	retained("water/hot/status", `"event":"HEARTBEAT"`)
	p.Close()
	retained("water/hot/status", `"event":"LWT"`)
	sig <- syscall.SIGTERM
	<-done
}
