// Package ingest is how washhouse's inputs reach the engine: the topics it
// subscribes to, decoding each message by its topic, the freshness gate a
// message passes before the engine sees it, and the engine call it makes.
//
// It knows tanktemp's state, status and meter, the boiler programmer's
// events and system topics, and the boiler-room climate sensor's topic
// (its payload has no timestamp, so its time is the receive time). The
// engine already orders its inputs in event time and skips replays; the
// gate adds what needs the wall clock (Gate).
//
// It reads no files and no network: the service and internal/replay hand
// it topics and payloads.
package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sweeney/common/wire/boiler"
	"github.com/sweeney/common/wire/tanktemp"
)

// Sources are the topics washhouse reads.
type Sources struct {
	// Tanktemp is tanktemp's topic prefix (water/hot/tanktemp). A recording
	// made before the move to water/hot also has energy/hot_water; the
	// service has one.
	Tanktemp []string
	Boiler   string // boiler-sensor's prefix: energy/boiler/sensor
	Ambient  string // the boiler-room climate topic; "" for none
}

// Validate refuses a missing prefix, a wildcard, and a trailing slash.
func (s Sources) Validate() error {
	if len(s.Tanktemp) == 0 {
		return errors.New("ingest: no tanktemp prefix")
	}
	for _, p := range s.Tanktemp {
		if err := topic("tanktemp prefix", p); err != nil {
			return err
		}
	}
	if err := topic("boiler prefix", s.Boiler); err != nil {
		return err
	}
	if s.Ambient != "" {
		return topic("ambient topic", s.Ambient)
	}
	return nil
}

func topic(what, t string) error {
	switch {
	case t == "":
		return fmt.Errorf("ingest: the %s is empty", what)
	case strings.ContainsAny(t, "#+"):
		return fmt.Errorf("ingest: the %s %q has a wildcard", what, t)
	case strings.HasPrefix(t, "/") || strings.HasSuffix(t, "/"):
		return fmt.Errorf("ingest: the %s %q starts or ends with /", what, t)
	}
	return nil
}

// Filters are the subscriptions: exactly the topics Decode reads, so a
// topic added later doesn't arrive unannounced, and never washhouse's own
// outputs.
func (s Sources) Filters() []string {
	var f []string
	for _, p := range s.Tanktemp {
		f = append(f, p+"/+/state", p+"/status", p+"/meter")
	}
	f = append(f, s.Boiler+"/events", s.Boiler+"/system")
	if s.Ambient != "" {
		f = append(f, s.Ambient)
	}
	return f
}

// Ambient is one boiler-room temperature reading.
type Ambient struct {
	At    time.Time // the receive time
	TempC float64
}

// Msg is one decoded message. Exactly one of the pointers is set.
type Msg struct {
	Received time.Time
	Topic    string

	State        *tanktemp.State
	Status       *tanktemp.Status
	Meter        *tanktemp.Meter
	BoilerEvent  *boiler.Event
	BoilerSystem *boiler.System
	Ambient      *Ambient
}

// Decode decodes a message received at received. ok is false for a topic
// it doesn't read, and for a climate message without a temperature; an
// error is a message on a topic it reads that doesn't decode.
func (s Sources) Decode(topic string, b []byte, received time.Time) (m Msg, ok bool, err error) {
	m = Msg{Received: received, Topic: topic}
	switch topic {
	case s.Boiler + "/events":
		e, err := boiler.DecodeEvent(b)
		m.BoilerEvent = &e
		return m, true, err
	case s.Boiler + "/system":
		x, err := boiler.DecodeSystem(b)
		m.BoilerSystem = &x
		return m, true, err
	}
	if s.Ambient != "" && topic == s.Ambient {
		var c struct {
			Temperature *float64 `json:"temperature"`
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return m, true, err
		}
		if c.Temperature == nil {
			return m, false, nil // nothing to use
		}
		m.Ambient = &Ambient{At: received, TempC: *c.Temperature}
		return m, true, nil
	}
	for _, p := range s.Tanktemp {
		rest, found := strings.CutPrefix(topic, p+"/")
		if !found {
			continue
		}
		switch {
		case rest == "meter":
			x, err := tanktemp.DecodeMeter(b)
			m.Meter = &x
			return m, true, err
		case rest == "status":
			x, err := tanktemp.DecodeStatus(b)
			m.Status = &x
			return m, true, err
		case strings.HasSuffix(rest, "/state") && !strings.Contains(strings.TrimSuffix(rest, "/state"), "/"):
			x, err := tanktemp.DecodeState(b)
			m.State = &x
			return m, true, err
		}
		return m, false, nil
	}
	return m, false, nil
}

// Gate is the freshness check a message passes before the engine sees it,
// against the wall clock. The engine runs on event time, and event time
// never goes back, so:
//
//   - a message dated more than Ahead past the wall clock is refused: one
//     bad timestamp would otherwise move event time on for good, ending
//     every draw and making every input stale. Only times the publisher
//     vouches for count: an unsynced sweep or meter message the engine
//     doesn't date by anyway.
//   - a retained tank sweep older than Stale is refused, as in statehouse's
//     tanktemp adapter: a retained message says nothing about its age, and
//     the next sweep is seconds away. Other retained inputs are taken:
//     the meter's is the baseline its litres count from, and the engine
//     orders the programmer's and tanktemp's status by their own times.
//
// The retained flag is only a hint (a bridge replays retained messages
// without it), so nothing here depends on its being set; a stale replay
// that arrives unflagged is an old sweep, which the engine skips.
type Gate struct {
	Ahead time.Duration
	Stale time.Duration
}

// Validate refuses a gate that would refuse everything.
func (g Gate) Validate() error {
	if g.Ahead <= 0 || g.Stale <= 0 {
		return fmt.Errorf("ingest: gate ahead %v and stale %v must both be more than 0", g.Ahead, g.Stale)
	}
	return nil
}

// Admit says why m, received when the wall clock read now, mustn't reach
// the engine, or nil if it may.
func (g Gate) Admit(m Msg, retained bool, now time.Time) error {
	at, dated := vouched(m)
	if !dated {
		return nil
	}
	if at.Sub(now) > g.Ahead {
		return fmt.Errorf("dated %s, more than %v ahead of the clock", at.Format(time.RFC3339), g.Ahead)
	}
	if retained && m.State != nil && now.Sub(at) > g.Stale {
		return fmt.Errorf("a retained sweep dated %s, more than %v old", at.Format(time.RFC3339), g.Stale)
	}
	return nil
}

// vouched is m's own time, if its publisher vouches for it.
func vouched(m Msg) (time.Time, bool) {
	switch {
	case m.State != nil:
		return m.State.Timestamp, m.State.ClockSynced
	case m.Meter != nil:
		return m.Meter.Timestamp, m.Meter.ClockSynced
	case m.Status != nil && m.Status.Timestamp != nil:
		return *m.Status.Timestamp, true
	case m.BoilerEvent != nil:
		return m.BoilerEvent.Timestamp, true
	case m.BoilerSystem != nil:
		return m.BoilerSystem.Timestamp, true
	}
	return time.Time{}, false // the room's time is the receive time; a last will has none
}

// Engine is what Feed drives: *engine.Engine, whose events are E. It is
// an interface so that ingest needn't import the engine, whose tests read
// fixtures through internal/replay, and so through ingest.
type Engine[E any] interface {
	TankState(tanktemp.State) ([]E, error)
	Meter(tanktemp.Meter) []E
	Status(tanktemp.Status)
	BoilerEvent(boiler.Event) []E
	BoilerSystem(boiler.System) []E
	Ambient(at time.Time, tempC float64) []E
}

// Feed gives one decoded message to the engine and returns its events.
func Feed[E any](e Engine[E], m Msg) ([]E, error) {
	switch {
	case m.State != nil:
		return e.TankState(*m.State)
	case m.Meter != nil:
		return e.Meter(*m.Meter), nil
	case m.Status != nil:
		e.Status(*m.Status)
		return nil, nil
	case m.BoilerEvent != nil:
		return e.BoilerEvent(*m.BoilerEvent), nil
	case m.BoilerSystem != nil:
		return e.BoilerSystem(*m.BoilerSystem), nil
	case m.Ambient != nil:
		return e.Ambient(m.Ambient.At, m.Ambient.TempC), nil
	}
	return nil, errors.New("ingest: a message with nothing decoded")
}
