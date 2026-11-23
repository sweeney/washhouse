package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sweeney/common/lifecycle"
	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/wire/washhouse"
	"github.com/sweeney/washhouse/internal/behaviour"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/ingest"
	"github.com/sweeney/washhouse/internal/publish"
)

// queueLen is how many received messages wait for the loop. A sweep is a
// few messages every 30 s, so a full queue means the loop is stuck.
const queueLen = 1024

// service is washhouse's loop: messages in, the engine, messages out. One
// goroutine (loop) owns the engine and the publisher; paho's handlers only
// queue (enqueue), since a handler that publishes and waits can deadlock
// paho, and one that blocks stalls every subscription.
type service struct {
	src   ingest.Sources
	gate  ingest.Gate
	fresh behaviour.Freshness
	eng   *engine.Engine
	pub   *publish.Publisher
	out   mqttpub.Publisher
	wall  func() time.Time
	log   *log.Logger
	base  string
	tanks []string

	in    chan mqttpub.Received
	ticks chan chan struct{}

	// Owned by the loop: the event time of the latest input that moved it
	// on, and the wall time it arrived, from which a silence moves event
	// time on; and when tanktemp's latest dated input arrived, from which
	// the silence is measured.
	lastEvent, lastWall time.Time
	lastTanktemp        time.Time

	start   time.Time
	version string
	ready   atomic.Bool // set once subscribed; never cleared (WIRE.md, ready)

	mu sync.Mutex
	h  healthStats
}

// healthStats are the loop's counts since start, for /healthz.
type healthStats struct {
	received, refused, decodeErrors, feedErrors, dropped int
	published, publishErrors                             int
	lastReceived                                         time.Time // wall clock
	lastTanktemp                                         time.Time // wall clock: tanktemp's latest dated input
	eventAt                                              time.Time // the engine's event time
}

func newService(lc localConfig, bc behaviour.Config, ec engine.Config, out mqttpub.Publisher, wall func() time.Time, logger *log.Logger) (*service, error) {
	if logger == nil {
		logger = log.New(log.Writer(), "", 0)
	}
	eng, err := engine.New(ec)
	if err != nil {
		return nil, err
	}
	pub, err := publish.New(lc.MQTT.BaseTopic)
	if err != nil {
		return nil, err
	}
	fr := bc.Fresh()
	s := &service{
		src: lc.sources(bc.AmbientTopic), gate: ingest.Gate{Ahead: fr.Ahead, Stale: fr.Stale}, fresh: fr,
		eng: eng, pub: pub, out: out, wall: wall, log: logger, base: lc.MQTT.BaseTopic,
		in: make(chan mqttpub.Received, queueLen), ticks: make(chan chan struct{}),
	}
	for _, t := range ec.Tanks {
		s.tanks = append(s.tanks, t.DeviceID)
	}
	if err := s.src.Validate(); err != nil {
		return nil, err
	}
	if err := s.gate.Validate(); err != nil {
		return nil, err
	}
	// Never read back what washhouse publishes.
	own := []string{washhouse.StateTopic(s.base), washhouse.EventsTopic(s.base), washhouse.StatusTopic(s.base)}
	for _, id := range s.tanks {
		own = append(own, washhouse.TankTopic(s.base, id))
	}
	for _, f := range s.src.Filters() {
		for _, t := range own {
			if mqttpub.Match(f, t) {
				return nil, fmt.Errorf("the subscription %s reads washhouse's own %s", f, t)
			}
		}
	}
	return s, nil
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
	st := washhouse.Status{Online: true, Event: washhouse.StatusHeartbeat, Timestamp: washhouse.Time{Time: e.At},
		Version: s.version, Ready: s.ready.Load(), UptimeS: int64(e.At.Sub(s.start) / time.Second), Tanks: s.tanks}
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
