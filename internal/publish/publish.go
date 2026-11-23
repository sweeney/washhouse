// Package publish turns the engine's state and events into washhouse's
// MQTT messages (docs/WIRE.md): the topics, the payloads, and when each
// retained topic goes out.
//
// It is pure: it builds messages, and the service sends them. Retained
// topics (the state and each tank) are published when their content
// changes, the timestamp aside; events always. Within one input the order
// is events, then tanks, then the state, so the state that drops a draw's
// active_draw follows its draw_ended.
package publish

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sweeney/common/wire/washhouse"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/thermal"
)

// Message is one MQTT publish.
type Message struct {
	Topic    string
	Payload  []byte
	Retained bool
	QoS      byte
}

// Publisher remembers what each retained topic last carried.
type Publisher struct {
	base string
	last map[string][]byte // topic → payload with a zero timestamp
}

// New returns a Publisher under base (e.g. water/hot). It refuses a base
// with a wildcard, an empty one, and one inside tanktemp's subtree.
func New(base string) (*Publisher, error) {
	base = strings.TrimSuffix(base, "/")
	switch {
	case base == "":
		return nil, errors.New("publish: base topic is empty")
	case strings.ContainsAny(base, "#+"):
		return nil, fmt.Errorf("publish: base topic %q has a wildcard", base)
	case strings.Contains("/"+base+"/", "/tanktemp/"):
		return nil, fmt.Errorf("publish: base topic %q is inside tanktemp's subtree", base)
	}
	return &Publisher{base: base, last: map[string][]byte{}}, nil
}

// After is what to publish after an input: s is the engine's state after
// it, ev the events it returned. Nothing is published before the engine
// has an input (s.At zero).
func (p *Publisher) After(s engine.State, ev []engine.Event) ([]Message, error) {
	if s.At.IsZero() {
		return nil, nil
	}
	var out []Message
	for _, e := range ev {
		m, err := Event(e, p.base)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	for _, t := range s.Tanks {
		m, err := p.retained(washhouse.TankTopic(p.base, t.DeviceID), s.At, func(at time.Time) ([]byte, error) {
			x := s
			x.At = at
			return Tank(x, t)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, m...)
	}
	m, err := p.retained(washhouse.StateTopic(p.base), s.At, func(at time.Time) ([]byte, error) {
		x := s
		x.At = at
		return State(x)
	})
	if err != nil {
		return nil, err
	}
	return append(out, m...), nil
}

// retained publishes topic if its content, encoded with a zero timestamp,
// differs from the last published.
func (p *Publisher) retained(topic string, at time.Time, enc func(time.Time) ([]byte, error)) ([]Message, error) {
	key, err := enc(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("publish: %s: %w", topic, err)
	}
	if last, ok := p.last[topic]; ok && bytes.Equal(last, key) {
		return nil, nil
	}
	b, err := enc(at)
	if err != nil {
		return nil, fmt.Errorf("publish: %s: %w", topic, err)
	}
	p.last[topic] = key
	return []Message{{Topic: topic, Payload: b, Retained: true}}, nil
}

func n(x *float64) *washhouse.Num {
	if x == nil {
		return nil
	}
	v := washhouse.Num(*x)
	return &v
}

func tm(t *time.Time) *washhouse.Time {
	if t == nil {
		return nil
	}
	return &washhouse.Time{Time: *t}
}

func src(s *thermal.Source) *washhouse.Source {
	if s == nil || *s == thermal.SourceNone {
		return nil
	}
	v := washhouse.Source(*s)
	return &v
}

func active(d *engine.ActiveDraw) *washhouse.ActiveDraw {
	if d == nil {
		return nil
	}
	return &washhouse.ActiveDraw{DrawID: d.DrawID, StartedAt: washhouse.Time{Time: d.StartedAt},
		LastLitreAt: washhouse.Time{Time: d.LastLitreAt}, Litres: washhouse.Num(d.LitresSoFar),
		FlowLPerMin: n(d.FlowLPerMin), Kind: washhouse.Kind(d.Kind)}
}

func flow(f *engine.Flow) *washhouse.ContinuousFlow {
	if f == nil {
		return nil
	}
	return &washhouse.ContinuousFlow{Since: washhouse.Time{Time: f.Since}, Last: washhouse.Time{Time: f.Last},
		Litres: washhouse.Num(f.Litres), FlowLPerMin: washhouse.Num(f.FlowLPerMin)}
}

// State is s's {base}/state payload.
func State(s engine.State) ([]byte, error) {
	w := washhouse.State{
		Timestamp: washhouse.Time{Time: s.At},
		StoredKWh: n(s.StoredKWh), UsableL40: n(s.UsableL40), UsableL40Low: n(s.UsableL40Low),
		LowHotWater: s.LowHotWater,
		Boiler:      washhouse.Boiler{HW: s.Boiler.HW, CH: s.Boiler.CH},
		AmbientC:    n(s.AmbientC),
		ActiveDraw:  active(s.ActiveDraw),
		Meter: washhouse.Meter{Timestamp: tm(s.Meter.At), CountingSince: tm(s.Meter.CountingSince),
			Count: s.Meter.Count, LitresSinceStart: n(s.Meter.Litres), ContinuousFlow: flow(s.Meter.ContinuousFlow)},
		Tanktemp: washhouse.Tanktemp{Timestamp: tm(s.Tanktemp.At), Online: s.Tanktemp.Online, Version: s.Tanktemp.Version},
		Tanks:    []washhouse.TankSummary{},
	}
	for _, t := range s.Tanks {
		w.Tanks = append(w.Tanks, washhouse.TankSummary{DeviceID: t.DeviceID, State: washhouse.TankState(t.State),
			HeatSource: src(t.HeatSource), StoredKWh: n(t.StoredKWh), UsableL40: n(t.UsableL40), TopC: n(t.TopC)})
	}
	return washhouse.EncodeState(w)
}

// Tank is t's {base}/tank/{device_id} payload, stamped with s's time.
func Tank(s engine.State, t engine.TankState) ([]byte, error) {
	return washhouse.EncodeTank(washhouse.Tank{
		DeviceID: t.DeviceID, Timestamp: washhouse.Time{Time: s.At}, SweepAt: tm(t.At),
		State: washhouse.TankState(t.State), HeatSource: src(t.HeatSource),
		StoredKWh: n(t.StoredKWh), MeanC: n(t.MeanC), TopC: n(t.TopC), HotL: n(t.HotL),
		UsableL40: n(t.UsableL40), UsableL40Low: n(t.UsableL40Low), PowerKW: n(t.PowerKW), UAWPerK: n(t.UAWPerK),
		ProbesOK: t.ProbesOK, ProbesTotal: t.ProbesTotal,
	})
}

// Event is e's {base}/events message.
func Event(e engine.Event, base string) (Message, error) {
	w := washhouse.Event{Event: washhouse.EventType(e.Type), Timestamp: washhouse.Time{Time: e.At},
		ActiveDraw: active(e.Draw), ContinuousFlow: flow(e.Flow)}
	if d := e.Ended; d != nil {
		w.DrawEnded = &washhouse.DrawEnded{DrawID: d.DrawID, StartedAt: washhouse.Time{Time: d.StartedAt},
			EndedAt: washhouse.Time{Time: d.EndedAt}, DurationS: washhouse.Num(d.DurationS),
			Litres: washhouse.Num(d.Litres), VolumeKind: d.VolumeKind, FlowLPerMin: n(d.FlowLPerMin),
			Kind:      washhouse.Kind(d.Kind),
			MeterFrom: washhouse.MeterRef{CountingSince: washhouse.Time{Time: d.From.CountingSince}, Seq: d.From.Seq},
			MeterTo:   washhouse.MeterRef{CountingSince: washhouse.Time{Time: d.To.CountingSince}, Seq: d.To.Seq}}
	}
	if d := e.Balanced; d != nil {
		b := &washhouse.DrawBalanced{DrawID: d.DrawID, EndedAt: washhouse.Time{Time: d.EndedAt},
			Tanks:       []washhouse.TankShare{},
			OutletPeakC: n(d.OutletPeakC), InletDropC: n(d.InletDropC), EnergyKWh: n(d.EnergyKWh),
			HeatBalanceL: n(d.HeatBalanceL), HeatBalanceReliable: d.HeatBalanceReliable,
			Reasons: []washhouse.Reason{}, MaskedByReheat: d.MaskedByReheat}
		for _, t := range d.Tanks {
			b.Tanks = append(b.Tanks, washhouse.TankShare{DeviceID: t.DeviceID, Share: n(t.Share), Litres: n(t.Litres),
				OutletPeakC: n(t.OutletPeakC), InletDropC: n(t.InletDropC)})
		}
		for _, r := range d.Reasons {
			b.Reasons = append(b.Reasons, washhouse.Reason(r))
		}
		w.DrawBalanced = b
	}
	if t := e.Tank; t != nil {
		w.Tank = &washhouse.TankEvent{DeviceID: t.DeviceID, State: washhouse.TankState(t.State),
			Prev: washhouse.TankState(t.Prev), HeatSource: src(t.Source), PowerKW: n(t.PowerKW)}
	}
	b, err := washhouse.EncodeEvent(w)
	if err != nil {
		return Message{}, fmt.Errorf("publish: %s: %w", e.Type, err)
	}
	return Message{Topic: washhouse.EventsTopic(base), Payload: b, QoS: 1}, nil
}
