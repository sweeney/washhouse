// Package influx turns washhouse's published messages (docs/WIRE.md) into
// Influx points, so the history is exactly what went out on the wire. It
// is pure: the service writes the points (cmd/washhouse/influx.go).
//
// The measurements (README, Influx), each tagged site, at event time:
//
//   - hot_water_tank {device_id}: each tank's retained message; blind is 1
//     while it has a fault, with fault_reason.
//   - hot_water_system: the state's totals and inputs; while a tank is
//     blind, tanks_missing and the partial_* lower bounds (the totals are
//     then absent).
//   - hot_water_draw: a row per draw at its ended_at; draw_ended writes the
//     metered fields and draw_balanced, minutes later, the cross-check, into
//     the same row (same series, same time, kind a field for that reason).
//   - hot_water_draw_tank {device_id}: each tank's part of a draw.
//   - hot_water_continuous_flow: a row per run of creep, at its since.
//
// An unknown value (null) is left out. A boolean is written as 0 or 1, so
// a mean over a window is the fraction of it (greenhouse's review); field
// types never change within a series (statehouse's review): quantities are
// float64 even when whole, counts int64, words strings.
package influx

import (
	"fmt"
	"strings"
	"time"

	"github.com/sweeney/common/wire/washhouse"
	"github.com/sweeney/washhouse/internal/publish"
)

// Point is one Influx point.
type Point struct {
	Measurement string
	Tags        map[string]string
	Fields      map[string]any // float64, int64 or string
	Time        time.Time
}

// Mapper maps the messages published under Base. Site is the site tag,
// left out when empty (an empty tag value would be a series of its own).
type Mapper struct {
	Base string
	Site string
}

// Points is the points for one published message: none for a status or
// an event the state and tanks already carry.
func (m Mapper) Points(msg publish.Message) ([]Point, error) {
	base := strings.TrimSuffix(m.Base, "/")
	switch {
	case msg.Topic == washhouse.StateTopic(base):
		s, err := washhouse.DecodeState(msg.Payload)
		if err != nil {
			return nil, fmt.Errorf("influx: %s: %w", msg.Topic, err)
		}
		return []Point{m.system(s)}, nil
	case strings.HasPrefix(msg.Topic, washhouse.TankTopic(base, "")):
		t, err := washhouse.DecodeTank(msg.Payload)
		if err != nil {
			return nil, fmt.Errorf("influx: %s: %w", msg.Topic, err)
		}
		return []Point{m.tank(t)}, nil
	case msg.Topic == washhouse.EventsTopic(base):
		e, err := washhouse.DecodeEvent(msg.Payload)
		if err != nil {
			return nil, fmt.Errorf("influx: %s: %w", msg.Topic, err)
		}
		return m.event(e), nil
	case msg.Topic == washhouse.StatusTopic(base):
		return nil, nil
	}
	return nil, fmt.Errorf("influx: %s is not one of washhouse's topics under %s", msg.Topic, base)
}

func (m Mapper) tags(kv ...string) map[string]string {
	t := map[string]string{}
	if m.Site != "" {
		t["site"] = m.Site
	}
	for i := 0; i+1 < len(kv); i += 2 {
		t[kv[i]] = kv[i+1]
	}
	return t
}

// fields builds a point's fields, leaving out the unknown.
type fields map[string]any

func (f fields) num(k string, v *washhouse.Num) {
	if v != nil {
		f[k] = float64(*v)
	}
}

func (f fields) count(k string, v *int) {
	if v != nil {
		f[k] = int64(*v)
	}
}

func (f fields) flag(k string, v *bool) {
	if v != nil {
		f[k] = bit(*v)
	}
}

func bit(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func (m Mapper) tank(t washhouse.Tank) Point {
	f := fields{"state": string(t.State)}
	if t.HeatSource != nil {
		f["heat_source"] = string(*t.HeatSource)
	}
	f.num("stored_kwh", t.StoredKWh)
	f.num("mean_c", t.MeanC)
	f.num("top_c", t.TopC)
	f.num("hot_l", t.HotL)
	f.num("usable_l40", t.UsableL40)
	f.num("usable_l40_low", t.UsableL40Low)
	f.num("power_kw", t.PowerKW)
	f.num("ua_w_per_k", t.UAWPerK)
	f.count("probes_ok", t.ProbesOK)
	f.count("probes_total", t.ProbesTotal)
	f["blind"] = bit(t.Fault != nil)
	if t.Fault != nil {
		f["fault_reason"] = string(t.Fault.Reason)
	}
	return Point{"hot_water_tank", m.tags("device_id", t.DeviceID), f, t.Timestamp.Time}
}

// system is the state's totals, and what washhouse saw of its inputs: the
// canonical series for the room and the programmer are statehouse's.
func (m Mapper) system(s washhouse.State) Point {
	f := fields{"drawing": bit(s.ActiveDraw != nil), "continuous_flow": bit(s.Meter.ContinuousFlow != nil)}
	f.num("stored_kwh", s.StoredKWh)
	f.num("usable_l40", s.UsableL40)
	f.num("usable_l40_low", s.UsableL40Low)
	f.flag("low_hot_water", s.LowHotWater)
	f.flag("boiler_hw", s.Boiler.HW)
	f.flag("boiler_ch", s.Boiler.CH)
	f.num("ambient_c", s.AmbientC)
	f.flag("tanktemp_online", s.Tanktemp.Online)
	// A partial is written apart from the totals, so a chart never draws a
	// lower bound as the real figure.
	missing := int64(0)
	for _, t := range s.Tanks {
		if t.StoredKWh == nil {
			missing++
		}
	}
	f["tanks_missing"] = missing
	if p := s.Partial; p != nil {
		f["partial_stored_kwh"] = float64(p.StoredKWh)
		f["partial_usable_l40"] = float64(p.UsableL40)
		f["partial_usable_l40_low"] = float64(p.UsableL40Low)
	}
	return Point{"hot_water_system", m.tags(), f, s.Timestamp.Time}
}

func (m Mapper) event(e washhouse.Event) []Point {
	switch {
	case e.Event == washhouse.EventDrawEnded && e.DrawEnded != nil:
		d := e.DrawEnded
		f := fields{"draw_id": d.DrawID, "kind": string(d.Kind), "litres": float64(d.Litres), "duration_s": float64(d.DurationS)}
		f.num("flow_l_min", d.FlowLPerMin)
		return []Point{{"hot_water_draw", m.tags(), f, d.EndedAt.Time}}
	case e.Event == washhouse.EventDrawBalanced && e.DrawBalanced != nil:
		d := e.DrawBalanced
		reasons := make([]string, len(d.Reasons))
		for i, r := range d.Reasons {
			reasons[i] = string(r)
		}
		f := fields{"draw_id": d.DrawID, "heat_balance_reliable": bit(d.HeatBalanceReliable),
			"masked_by_reheat": bit(d.MaskedByReheat), "reasons": strings.Join(reasons, ",")}
		f.num("outlet_peak_c", d.OutletPeakC)
		f.num("inlet_drop_c", d.InletDropC)
		f.num("energy_kwh", d.EnergyKWh)
		f.num("heat_balance_l", d.HeatBalanceL)
		out := []Point{{"hot_water_draw", m.tags(), f, d.EndedAt.Time}}
		for _, t := range d.Tanks {
			tf := fields{"draw_id": d.DrawID}
			tf.num("share", t.Share)
			tf.num("litres", t.Litres)
			tf.num("outlet_peak_c", t.OutletPeakC)
			tf.num("inlet_drop_c", t.InletDropC)
			out = append(out, Point{"hot_water_draw_tank", m.tags("device_id", t.DeviceID), tf, d.EndedAt.Time})
		}
		return out
	case e.Event == washhouse.EventContinuousFlowEnded && e.ContinuousFlow != nil:
		c := e.ContinuousFlow
		f := fields{"litres": float64(c.Litres), "flow_l_min": float64(c.FlowLPerMin),
			"duration_s": c.Last.Sub(c.Since.Time).Seconds()}
		return []Point{{"hot_water_continuous_flow", m.tags(), f, c.Since.Time}}
	}
	return nil
}
