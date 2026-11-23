// Command washhouse turns tanktemp's probe and meter messages, and the
// boiler programmer's, into hot water state and draw events on MQTT
// (docs/WIRE.md), with /healthz over HTTP.
//
//	washhouse -config /etc/washhouse/config.yaml
//	washhouse -config /etc/washhouse/config.yaml -check-config
//	washhouse -check-behaviour behaviour.json
//	washhouse -version
//
// The local config (deploy/config.example.yaml) says how to reach the
// broker and where things are on it; the behaviour document
// (testdata/behaviour.example.json) says everything else. That is the
// config service's namespace: washhouse refuses to start without it, and
// SIGHUP fetches it again, keeping the running one if it is refused.
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
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sweeney/common/health"
	"github.com/sweeney/common/lifecycle"
	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/retry"
	"github.com/sweeney/common/remoteconfig"
	"github.com/sweeney/common/wire/washhouse"
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
	// bootWait bounds the first fetch of the behaviour namespace.
	bootWait = 30 * time.Second
)

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }

func realMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("washhouse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "/etc/washhouse/config.yaml", "the local config file")
	check := fs.Bool("check-config", false, "check the config and fetch the behaviour namespace, then exit")
	checkBehaviour := fs.String("check-behaviour", "", "check a behaviour document `file` (before writing it to the namespace), then exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "washhouse %s\n", version)
		return 0
	}
	if *checkBehaviour != "" {
		b, err := os.ReadFile(*checkBehaviour)
		if err == nil {
			var d document
			if d, err = parseDocument(b); err == nil {
				fmt.Fprintf(stdout, "%s: ok: %s, room %s\n", *checkBehaviour, describe(d), d.bc.AmbientTopic)
				return 0
			}
		}
		fmt.Fprintf(stderr, "washhouse: %s: %v\n", *checkBehaviour, err)
		return 1
	}
	// SIGHUP's default is to exit: catch it before anything can take time.
	var hup chan os.Signal
	if !*check {
		hup = make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
	}
	lc, err := readConfig(*path)
	if err != nil {
		fmt.Fprintf(stderr, "washhouse: %v\n", err)
		return 1
	}
	r, err := newRemote(lc, version)
	if err != nil {
		fmt.Fprintf(stderr, "washhouse: %v\n", err)
		return 1
	}
	// Longer than a reload's wait: right after a host reboot, identity and
	// the config service may be up but slow.
	ctx, cancel := context.WithTimeout(context.Background(), bootWait)
	d, err := r.fetch(ctx)
	cancel()
	if err != nil {
		fmt.Fprintf(stderr, "washhouse: the behaviour namespace from %s: %v; refusing to start without it\n", r.base, err)
		return 1
	}
	if *check {
		fmt.Fprintf(stdout, "%s: ok: %s, reading %s and %s, publishing to %s on %s, behaviour from %s on %s\n",
			*path, describe(d), lc.MQTT.TanktempTopic, lc.MQTT.BoilerTopic, lc.MQTT.BaseTopic, lc.MQTT.Broker, r.ns, r.base)
		return 0
	}
	if err := serve(lc, r, d, hup); err != nil {
		fmt.Fprintf(stderr, "washhouse: %v\n", err)
		return 1
	}
	return 0
}

func describe(d document) string {
	var ids []string
	for _, t := range d.ec.Tanks {
		ids = append(ids, t.DeviceID)
	}
	return fmt.Sprintf("%d tanks (%s), meter %s", len(ids), strings.Join(ids, ", "), d.ec.Meter.DeviceID)
}

// readConfig reads the local config.
func readConfig(path string) (localConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return localConfig{}, err
	}
	lc, err := loadLocal(b)
	if err != nil {
		return localConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return lc, nil
}

// serve builds the real dependencies and runs.
func serve(lc localConfig, r *remote, d document, hup <-chan os.Signal) error {
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
	s, err := newService(lc, d, pub, time.Now, logger)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", lc.HTTP.Listen)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	return run(context.Background(), s, deps{Conn: pub, Wall: time.Now, Listener: ln, Logger: logger,
		Version: version, Heartbeat: time.Duration(lc.Heartbeat), Broker: lc.MQTT.Broker, ClientID: id,
		Remote: r, Reload: hup})
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

	Remote *remote          // the behaviour namespace; nil: none
	Reload <-chan os.Signal // each a SIGHUP: fetch Remote again
}

// run is washhouse: subscribe, connect, then the loop under lifecycle
// until a signal; SHUTDOWN is the last status.
func run(ctx context.Context, s *service, d deps) error {
	s.version, s.start, s.remote = d.Version, d.Wall(), d.Remote
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
	if d.Remote != nil && d.Reload != nil {
		loopDone.Add(1)
		go func() {
			defer loopDone.Done()
			for {
				select {
				case <-loopCtx.Done():
					return
				case <-d.Reload:
					s.reloadFrom(loopCtx, d.Remote)
				}
			}
		}()
	}

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
	case now.Sub(h.lastTanktemp) > s.silence():
		why = append(why, fmt.Sprintf("no tanktemp input for %v", now.Sub(h.lastTanktemp).Round(time.Second)))
	}
	if h.dropped > 0 {
		why = append(why, fmt.Sprintf("%d inputs dropped", h.dropped))
	}
	var namespaces map[string]remoteconfig.Status
	if s.remote != nil {
		namespaces = s.remote.tracker.Statuses()
		if st := s.remote.status(); !st.OK && !st.FetchedAt.IsZero() {
			why = append(why, "the behaviour namespace was refused (running the last good): "+st.Error)
		}
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
		{Key: "remote_config", Value: namespaces},
	}
	return r
}
