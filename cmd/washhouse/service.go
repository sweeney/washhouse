package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sweeney/common/lifecycle"
	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/wire/washhouse"
	"github.com/sweeney/washhouse/internal/behaviour"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/httpapi"
	"github.com/sweeney/washhouse/internal/ingest"
	"github.com/sweeney/washhouse/internal/publish"
)

// queueLen is how many received messages wait for the loop. A sweep is a
// few messages every 30 s, so a full queue means the loop is stuck.
const queueLen = 1024

// reloadWait bounds a SIGHUP's fetch.
const reloadWait = 20 * time.Second

// service is washhouse's loop: messages in, the engine, messages out. One
// goroutine (loop) owns the engine and the publisher; paho's handlers only
// queue (enqueue), since a handler that publishes and waits can deadlock
// paho, and one that blocks stalls every subscription.
type service struct {
	src  ingest.Sources // fixed at start: the subscriptions are
	out  mqttpub.Publisher
	wall func() time.Time
	log  *log.Logger
	base string

	// running is the behaviour document's part, owned by the loop and
	// replaced whole by apply.
	running

	remote *remote           // nil: no namespace (tests)
	hist   *influxSink       // nil: no history; the loop writes
	rec    *httpapi.Recorder // what the API serves; the loop records

	in      chan mqttpub.Received
	ticks   chan chan struct{}
	reloads chan reloadReq

	// Owned by the loop: the event time of the latest input that moved it
	// on, and the wall time it arrived, from which a silence moves event
	// time on; and when tanktemp's latest dated input arrived, from which
	// the silence is measured.
	lastEvent, lastWall time.Time
	lastTanktemp        time.Time
	// The latest admitted input on each topic, which a reload replays
	// into the new engine as a restart's retained messages would.
	latest map[string]admitted
	seq    int

	start   time.Time
	version string
	ready   atomic.Bool // set once subscribed; never cleared (WIRE.md, ready)

	mu sync.Mutex
	h  healthStats
	// What other goroutines read of running: the status's tank list
	// (lifecycle's), and the silence (/healthz's).
	shownTanks   []string
	shownSilence time.Duration
	appliedAt    time.Time // wall clock: when the running document was applied
}

// running is what a behaviour document makes: the engine and everything
// else a reload replaces.
type running struct {
	raw   []byte
	gate  ingest.Gate
	fresh behaviour.Freshness
	eng   *engine.Engine
	pub   *publish.Publisher
	tanks []string
}

type admitted struct {
	seq int
	m   ingest.Msg
}

type reloadReq struct {
	d    document
	done chan reloadResult
}

type reloadResult struct {
	changed bool
	err     error
}

// healthStats are the loop's counts since start, for /healthz.
type healthStats struct {
	received, refused, decodeErrors, feedErrors, dropped int
	published, publishErrors                             int
	lastReceived                                         time.Time // wall clock
	lastTanktemp                                         time.Time // wall clock: tanktemp's latest dated input
	eventAt                                              time.Time // the engine's event time
}

func newService(lc localConfig, d document, out mqttpub.Publisher, wall func() time.Time, logger *log.Logger) (*service, error) {
	if logger == nil {
		logger = log.New(log.Writer(), "", 0)
	}
	s := &service{
		src: lc.sources(d.bc.AmbientTopic), out: out, wall: wall, log: logger, base: lc.MQTT.BaseTopic,
		in: make(chan mqttpub.Received, queueLen), ticks: make(chan chan struct{}), reloads: make(chan reloadReq),
		latest: map[string]admitted{},
	}
	if err := s.src.Validate(); err != nil {
		return nil, err
	}
	r, err := s.build(d)
	if err != nil {
		return nil, err
	}
	s.rec = httpapi.NewRecorder(d.ec.RecentDraws)
	s.install(r, d.ec.RecentDraws)
	return s, nil
}

// build makes what d calls for, checked: never reading back what washhouse
// publishes.
func (s *service) build(d document) (running, error) {
	eng, err := engine.New(d.ec)
	if err != nil {
		return running{}, err
	}
	pub, err := publish.New(s.base)
	if err != nil {
		return running{}, err
	}
	fr := d.bc.Fresh()
	r := running{raw: d.raw, gate: ingest.Gate{Ahead: fr.Ahead, Stale: fr.Stale}, fresh: fr, eng: eng, pub: pub}
	for _, t := range d.ec.Tanks {
		r.tanks = append(r.tanks, t.DeviceID)
	}
	if err := r.gate.Validate(); err != nil {
		return running{}, err
	}
	own := []string{washhouse.StateTopic(s.base), washhouse.EventsTopic(s.base), washhouse.StatusTopic(s.base)}
	for _, id := range r.tanks {
		own = append(own, washhouse.TankTopic(s.base, id))
	}
	for _, f := range s.src.Filters() {
		for _, t := range own {
			if mqttpub.Match(f, t) {
				return running{}, fmt.Errorf("the subscription %s reads washhouse's own %s", f, t)
			}
		}
	}
	return r, nil
}

func (s *service) install(r running, keep int) {
	now := s.wall()
	s.running = r
	s.rec.Behaviour(r.raw, now, keep)
	s.mu.Lock()
	s.shownTanks, s.shownSilence, s.appliedAt = r.tanks, r.fresh.Silence, now
	s.mu.Unlock()
}

// apply is a reload, on the loop: an unchanged document changes nothing;
// a changed one starts the engine again (a restart, for consumers: what
// it has learnt goes, and a draw in progress gets no draw_ended), fed the
// latest input on each topic, in the order they came, as a restart's
// retained messages would be; it publishes nothing until the next input.
// A document that needs a restart is refused whole.
func (s *service) apply(d document) (bool, error) {
	if bytes.Equal(d.raw, s.raw) {
		return false, nil
	}
	if d.bc.AmbientTopic != s.src.Ambient {
		return false, fmt.Errorf("ambient_topic is %q, was %q: the subscriptions are made at start, so restart washhouse to apply it",
			d.bc.AmbientTopic, s.src.Ambient)
	}
	r, err := s.build(d)
	if err != nil {
		return false, err
	}
	ins := make([]admitted, 0, len(s.latest))
	for _, a := range s.latest {
		ins = append(ins, a)
	}
	sort.Slice(ins, func(i, j int) bool { return ins[i].seq < ins[j].seq })
	for _, a := range ins {
		if _, err := ingest.Feed[engine.Event](r.eng, a.m); err != nil {
			s.log.Printf("reload: replaying %s: %v", a.m.Topic, err)
		}
	}
	s.install(r, d.ec.RecentDraws)
	return true, nil
}

// reload has the loop apply d, and waits for its answer.
func (s *service) reload(ctx context.Context, d document) (bool, error) {
	req := reloadReq{d: d, done: make(chan reloadResult, 1)}
	select {
	case s.reloads <- req:
	case <-ctx.Done():
		return false, errNoLoop
	}
	select {
	case r := <-req.done:
		return r.changed, r.err
	case <-ctx.Done():
		return false, errNoLoop
	}
}

// reloadFrom is a SIGHUP: fetch the namespace and apply it, or keep the
// running behaviour.
func (s *service) reloadFrom(ctx context.Context, r *remote) {
	fctx, cancel := context.WithTimeout(ctx, reloadWait)
	d, err := r.fetch(fctx)
	cancel()
	if err != nil {
		s.log.Printf("reload: %v; keeping the running behaviour", err)
		return
	}
	changed, err := s.reload(ctx, d)
	switch {
	case errors.Is(err, errNoLoop):
	case err != nil:
		r.refused(err)
		s.log.Printf("reload: %v; keeping the running behaviour", err)
	case changed:
		s.log.Printf("behaviour reloaded from %s: the engine starts again", r.ns)
	default:
		s.log.Printf("behaviour unchanged")
	}
}

func (s *service) silence() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shownSilence
}

// subscribe makes every subscription at QoS 1: the broker sends each
// message at the lower of that and its publisher's (tanktemp's state and
// meter go at 0, its status and the programmer's events at 1).
func (s *service) subscribe(sub mqttpub.Subscriber) error {
	for _, f := range s.src.Filters() {
		if err := sub.Subscribe(f, 1, s.enqueue, s.lost); err != nil {
			return err
		}
	}
	return nil
}

// enqueue is every subscription's handler: it never blocks.
func (s *service) enqueue(r mqttpub.Received) {
	select {
	case s.in <- r:
	default:
		s.mu.Lock()
		s.h.dropped++
		n := s.h.dropped
		s.mu.Unlock()
		if n == 1 || n%100 == 0 {
			s.log.Printf("input queue full: %d messages dropped so far", n)
		}
	}
}

func (s *service) lost() { s.log.Printf("MQTT connection lost; inputs are missed until it is back") }

// loop runs until ctx is done: each message, each tick, and a HEARTBEAT
// after each reconnect (the broker published the will when the
// connection dropped).
func (s *service) loop(ctx context.Context, reconnected <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-s.in:
			s.handle(r)
		case done := <-s.ticks:
			s.tick()
			close(done)
		case req := <-s.reloads:
			changed, err := s.apply(req.d)
			req.done <- reloadResult{changed, err}
		case <-reconnected:
			s.emit(lifecycle.Event{Kind: lifecycle.Heartbeat, At: s.wall()})
		}
	}
}

// requestTick is lifecycle's Work: a tick, run on the loop.
func (s *service) requestTick(ctx context.Context) {
	done := make(chan struct{})
	select {
	case s.ticks <- done:
	case <-ctx.Done():
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// handle takes one message: decode, the gate, the engine, publish.
func (s *service) handle(r mqttpub.Received) {
	now := s.wall()
	s.mu.Lock()
	s.h.received++
	s.h.lastReceived = now
	s.mu.Unlock()
	m, ok, err := s.src.Decode(r.Topic, r.Payload, now)
	if err != nil {
		s.count(func(h *healthStats) { h.decodeErrors++ })
		s.log.Printf("%s: %v", r.Topic, err)
		return
	}
	if !ok {
		return
	}
	if err := s.gate.Admit(m, r.Retained, now); err != nil {
		s.count(func(h *healthStats) { h.refused++ })
		s.log.Printf("%s: refused: %v", r.Topic, err)
		return
	}
	// Kept whatever the engine makes of it: it is the topic's present, as
	// the retained message would be.
	s.seq++
	s.latest[r.Topic] = admitted{s.seq, m}
	ev, err := ingest.Feed[engine.Event](s.eng, m)
	if err != nil {
		s.count(func(h *healthStats) { h.feedErrors++ })
		s.log.Printf("%s: %v", r.Topic, err)
	}
	st := s.eng.State()
	if st.At.After(s.lastEvent) {
		s.lastEvent, s.lastWall = st.At, now
	}
	if (m.State != nil && m.State.ClockSynced) || (m.Meter != nil && m.Meter.ClockSynced) {
		s.lastTanktemp = now
		s.count(func(h *healthStats) { h.lastTanktemp = now })
	}
	s.send(st, ev)
}

// send publishes what st and ev call for, and notes the event time.
func (s *service) send(st engine.State, ev []engine.Event) {
	s.count(func(h *healthStats) { h.eventAt = st.At })
	s.publish(st, ev)
	if err := s.rec.After(st, ev); err != nil {
		s.log.Printf("api: %v", err)
	}
}

// tick moves event time on once tanktemp has been silent for the silence
// (no synced sweep or meter message; a room reading moves event time but
// ends no draw, PR #5 review): to the latest input's time plus the wall
// time since it arrived, so a draw still ends and the tanks still go
// stale when tanktemp has stopped. Never before the first input.
func (s *service) tick() {
	if s.lastWall.IsZero() {
		return
	}
	now := s.wall()
	if !s.lastTanktemp.IsZero() && now.Sub(s.lastTanktemp) < s.fresh.Silence {
		return
	}
	ev := s.eng.Tick(s.lastEvent.Add(now.Sub(s.lastWall)))
	s.send(s.eng.State(), ev)
}

func (s *service) publish(st engine.State, ev []engine.Event) {
	msgs, err := s.pub.After(st, ev)
	if err != nil {
		s.count(func(h *healthStats) { h.publishErrors++ })
		s.log.Printf("publish: %v", err)
		return
	}
	s.hist.write(msgs) // what washhouse found, whether or not the broker took it
	for _, m := range msgs {
		err := s.out.Publish(mqttpub.Message{Topic: m.Topic, Payload: string(m.Payload), Retained: m.Retained, QOS: m.QoS})
		s.count(func(h *healthStats) {
			if err != nil {
				h.publishErrors++
			} else {
				h.published++
			}
		})
		if err != nil {
			s.log.Printf("publish %s: %v", m.Topic, err)
		}
	}
}

func (s *service) count(f func(*healthStats)) {
	s.mu.Lock()
	f(&s.h)
	s.mu.Unlock()
}

func (s *service) health() healthStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.h
}

// status is e's {base}/status message. Ready (the lifecycle moment) is
// the first HEARTBEAT, as for tanktemp.
func (s *service) status(e lifecycle.Event) (mqttpub.Message, error) {
	s.mu.Lock()
	tanks := s.shownTanks
	s.mu.Unlock()
	st := washhouse.Status{Online: true, Event: washhouse.StatusHeartbeat, Timestamp: washhouse.Time{Time: e.At},
		Version: s.version, Ready: s.ready.Load(), UptimeS: int64(e.At.Sub(s.start) / time.Second), Tanks: tanks}
	switch e.Kind {
	case lifecycle.Startup:
		st.Event = washhouse.StatusStartup
	case lifecycle.Shutdown:
		st.Event, st.Online = washhouse.StatusShutdown, false
		r := e.Reason
		st.Reason = &r
	}
	b, err := washhouse.EncodeStatus(st)
	if err != nil {
		return mqttpub.Message{}, err
	}
	return mqttpub.Message{Topic: washhouse.StatusTopic(s.base), Payload: string(b), Retained: true, QOS: 1}, nil
}

// emit publishes e's status; lifecycle's Emit.
func (s *service) emit(e lifecycle.Event) error {
	m, err := s.status(e)
	if err != nil {
		return err
	}
	return s.out.Publish(m)
}
