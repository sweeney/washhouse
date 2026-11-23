// Command washhouse turns tanktemp's probe and meter messages, and the
// boiler programmer's, into hot water state and draw events on MQTT
// (docs/WIRE.md), with /healthz over HTTP.
//
//	washhouse -config /etc/washhouse/config.yaml
//	washhouse -config /etc/washhouse/config.yaml -check-config
//	washhouse -version
//
// The local config (deploy/config.example.yaml) says how to reach the
// broker and where things are on it; the behaviour document it names
// (testdata/behaviour.example.json) says everything else.
//
// This file only wires things together: the loop is service.go, the logic
// is in internal/, and run is tested with every outside dependency faked.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sweeney/common/health"
	"github.com/sweeney/common/lifecycle"
	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/retry"
	"github.com/sweeney/common/wire/washhouse"
	"github.com/sweeney/washhouse/internal/behaviour"
	"github.com/sweeney/washhouse/internal/engine"
)

// version is stamped at build time with -ldflags "-X main.version=<commit>".
var version = "dev"

const (
	// connectWait is how long startup waits for the broker before running
	// anyway, so STARTUP normally reaches it, while /healthz answers even
	// when the broker is down.
	connectWait = 10 * time.Second
	// tickEvery is how often the loop checks for a silence (service.tick).
	tickEvery = 5 * time.Second
)

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }

func realMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("washhouse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "/etc/washhouse/config.yaml", "the local config file")
	check := fs.Bool("check-config", false, "check the config and the behaviour document, then exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "washhouse %s\n", version)
		return 0
	}
	lc, bc, ec, err := readConfig(*path)
	if err != nil {
		fmt.Fprintf(stderr, "washhouse: %v\n", err)
		return 1
	}
	if *check {
		var ids []string
		for _, t := range ec.Tanks {
			ids = append(ids, t.DeviceID)
		}
		fmt.Fprintf(stdout, "%s: ok: %d tanks (%s), meter %s, reading %s and %s, publishing to %s on %s\n",
			*path, len(ids), strings.Join(ids, ", "), ec.Meter.DeviceID, lc.MQTT.TanktempTopic, lc.MQTT.BoilerTopic,
			lc.MQTT.BaseTopic, lc.MQTT.Broker)
		return 0
	}
	if err := serve(lc, bc, ec); err != nil {
		fmt.Fprintf(stderr, "washhouse: %v\n", err)
		return 1
	}
	return 0
}

// readConfig reads the local config and the behaviour document it names.
func readConfig(path string) (localConfig, behaviour.Config, engine.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return localConfig{}, behaviour.Config{}, engine.Config{}, err
	}
	lc, err := loadLocal(b)
	if err != nil {
		return localConfig{}, behaviour.Config{}, engine.Config{}, fmt.Errorf("%s: %w", path, err)
	}
	f, err := os.Open(lc.BehaviourFile)
	if err != nil {
		return localConfig{}, behaviour.Config{}, engine.Config{}, err
	}
	defer f.Close()
	bc, ec, err := behaviour.Load(f)
	if err != nil {
		return localConfig{}, behaviour.Config{}, engine.Config{}, fmt.Errorf("%s: %w", lc.BehaviourFile, err)
	}
	return lc, bc, ec, nil
}

// serve builds the real dependencies and runs.
func serve(lc localConfig, bc behaviour.Config, ec engine.Config) error {
	logger := log.New(os.Stderr, "", 0) // journald adds timestamps
	id := lc.MQTT.ClientID
	if id == "" {
		host, _ := os.Hostname()
		id = "washhouse-" + host
	}
	password := ""
	if lc.MQTT.PasswordFile != "" {
		b, err := os.ReadFile(lc.MQTT.PasswordFile)
		if err != nil {
			return fmt.Errorf("mqtt password file: %w", err)
		}
		password = strings.TrimSpace(string(b))
	}
	pub, err := mqttpub.NewMQTTPublisher(mqttpub.Options{Broker: lc.MQTT.Broker, ClientID: id,
		Username: lc.MQTT.Username, Password: password, WillQOS: 1},
		washhouse.StatusTopic(lc.MQTT.BaseTopic), string(washhouse.LWT()))
	if err != nil {
		return err
	}
	s, err := newService(lc, bc, ec, pub, time.Now, logger)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", lc.HTTP.Listen)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	return run(context.Background(), s, deps{Conn: pub, Wall: time.Now, Listener: ln, Logger: logger,
		Version: version, Heartbeat: time.Duration(lc.Heartbeat), Broker: lc.MQTT.Broker, ClientID: id})
}

// connection is what run needs from MQTT: MQTTPublisher, faked in tests.
type connection interface {
	mqttpub.Publisher
	mqttpub.Subscriber
	Connect(context.Context) error
	Connected() bool
	Reconnected() <-chan struct{}
}

// deps is everything run takes from outside. Tests fake all of it.
type deps struct {
	Conn     connection
	Wall     func() time.Time
	Listener net.Listener // nil: no HTTP
	Logger   *log.Logger
	Version  string

	Heartbeat        time.Duration    // when HeartbeatTick is nil
	Tick             <-chan time.Time // nil: every tickEvery
	HeartbeatTick    <-chan time.Time
	Signals          <-chan os.Signal // nil: SIGINT and SIGTERM
	Broker, ClientID string           // for the log
}

// run is washhouse: subscribe, connect, then the loop under lifecycle
// until a signal; SHUTDOWN is the last status.
func run(ctx context.Context, s *service, d deps) error {
	s.version, s.start = d.Version, d.Wall()
	if err := s.subscribe(d.Conn); err != nil {
		return err
	}
	defer d.Conn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	connected := make(chan struct{})
	go func() {
		_, err := retry.Connect(ctx, "MQTT broker "+d.Broker, mqttpub.DefaultTuning().Reconnect,
			func() error { return d.Conn.Connect(ctx) }, retry.Sleep)
		if err == nil {
			d.Logger.Printf("connected to %s as %s", d.Broker, d.ClientID)
			close(connected)
		}
	}()
	select {
	case <-connected:
	case <-time.After(connectWait):
		d.Logger.Printf("MQTT not connected after %v; running anyway", connectWait)
	}

	if d.Listener != nil {
		mux := http.NewServeMux()
		mux.Handle("GET /healthz", health.Handler(func() health.Report { return s.report(d.Conn.Connected(), d.Wall()) }))
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.Serve(d.Listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				d.Logger.Printf("http: %v", err)
			}
		}()
		defer func() {
			sctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			_ = srv.Shutdown(sctx)
		}()
	}

	// The loop starts with the first unit of work, after STARTUP, so no
	// state goes out before it; messages queue until then.
	loopCtx, stopLoop := context.WithCancel(context.Background())
	var once sync.Once
	var loopDone sync.WaitGroup
	defer func() { stopLoop(); loopDone.Wait() }()

	err := lifecycle.Run(ctx, lifecycle.Config{
		Work: func(wctx context.Context, _ time.Time) error {
			once.Do(func() {
				loopDone.Add(1)
				go func() { defer loopDone.Done(); s.loop(loopCtx, d.Conn.Reconnected()) }()
			})
			if d.Conn.Connected() {
				s.ready.Store(true) // subscribed: the subscriptions are made on connect
			}
			s.requestTick(wctx)
			return nil
		},
		Ready:         s.ready.Load,
		Emit:          s.emit,
		Interval:      tickEvery,
		Heartbeat:     d.Heartbeat,
		Immediate:     true,
		Now:           d.Wall,
		Tick:          d.Tick,
		HeartbeatTick: d.HeartbeatTick,
		Signals:       d.Signals,
		Logger:        d.Logger,
	})
	return err
}

// report is /healthz: starting until the first input, degraded while the
// broker is unreachable, while inputs are being dropped, or while tanktemp
// is silent (no synced sweep or meter message; the room and the
// programmer don't count); ok otherwise.
func (s *service) report(connected bool, now time.Time) health.Report {
	h := s.health()
	r := health.Report{Status: health.StatusOK, Version: s.version, Start: s.start, Now: now}
	var why []string
	switch {
	case !connected:
		why = append(why, "MQTT not connected")
	case h.lastReceived.IsZero():
		r.Status = health.StatusStarting
	case h.lastTanktemp.IsZero():
		why = append(why, "no tanktemp input yet")
	case now.Sub(h.lastTanktemp) > s.fresh.Silence:
		why = append(why, fmt.Sprintf("no tanktemp input for %v", now.Sub(h.lastTanktemp).Round(time.Second)))
	}
	if h.dropped > 0 {
		why = append(why, fmt.Sprintf("%d inputs dropped", h.dropped))
	}
	if len(why) > 0 {
		r.Status, r.Detail = health.StatusDegraded, strings.Join(why, "; ")
	}
	var eventAt, lastAt, lastTT *string
	if !h.lastTanktemp.IsZero() {
		x := h.lastTanktemp.UTC().Format(time.RFC3339)
		lastTT = &x
	}
	if !h.eventAt.IsZero() {
		x := h.eventAt.UTC().Format(time.RFC3339Nano)
		eventAt = &x
	}
	if !h.lastReceived.IsZero() {
		x := h.lastReceived.UTC().Format(time.RFC3339)
		lastAt = &x
	}
	r.Fields = []health.Field{
		{Key: "mqtt_connected", Value: connected},
		{Key: "ready", Value: s.ready.Load()},
		{Key: "event_time", Value: eventAt},
		{Key: "last_input_at", Value: lastAt},
		{Key: "last_tanktemp_at", Value: lastTT},
		{Key: "inputs", Value: map[string]int{"received": h.received, "refused": h.refused,
			"decode_errors": h.decodeErrors, "feed_errors": h.feedErrors, "dropped": h.dropped}},
		{Key: "published", Value: h.published},
		{Key: "publish_errors", Value: h.publishErrors},
	}
	return r
}
