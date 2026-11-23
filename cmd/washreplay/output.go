package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/publish"
	"github.com/sweeney/washhouse/internal/thermal"
)

// printer writes events and states, as text or JSON lines. Numbers are
// rounded (text: 1–2 decimals; JSON: 3), so the output doesn't hang on the
// last bits of a float, which can differ between platforms (arm64 fuses
// multiply-adds that amd64 doesn't).
type printer struct {
	w     io.Writer
	json  bool
	every time.Duration
	next  time.Time          // the next state line is due at or after this
	pub   *publish.Publisher // -wire: print the messages it builds instead
	err   error
}

// wire prints what the publisher builds after an input.
func (p *printer) wire(s engine.State, ev []engine.Event) {
	ms, err := p.pub.After(s, ev)
	if err != nil {
		if p.err == nil {
			p.err = err
		}
		return
	}
	for _, m := range ms {
		p.line(m.Topic + " " + string(m.Payload))
	}
}

const stamp = "2006-01-02T15:04:05.000Z"

func ts(t time.Time) string { return t.UTC().Format(stamp) }

func (p *printer) line(s string) {
	if p.err == nil {
		_, p.err = io.WriteString(p.w, s+"\n")
	}
}

func (p *printer) events(ev []engine.Event) {
	for _, e := range ev {
		if p.json {
			p.jsonLine(eventJSON(e))
		} else {
			p.line(ts(e.At) + " " + eventText(e))
		}
	}
}

// maybeState prints s if a state line is due: every p.every of event
// time, on the first input at or after each multiple of it.
func (p *printer) maybeState(s engine.State) {
	if p.every <= 0 || s.At.IsZero() {
		return
	}
	if p.next.IsZero() {
		p.next = s.At.Truncate(p.every).Add(p.every)
		return
	}
	if !s.At.Before(p.next) {
		p.state(s)
		p.next = s.At.Truncate(p.every).Add(p.every)
	}
}

func (p *printer) state(s engine.State) {
	if p.json {
		p.jsonLine(stateJSON(s))
	} else {
		p.line(ts(s.At) + " " + stateText(s))
	}
}

func (p *printer) jsonLine(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		if p.err == nil {
			p.err = err
		}
		return
	}
	p.line(string(b))
}

// --- text

func num(x *float64, prec int) string {
	if x == nil {
		return "-"
	}
	return strconv.FormatFloat(*x, 'f', prec, 64)
}

func onOff(b *bool) string {
	switch {
	case b == nil:
		return "-"
	case *b:
		return "on"
	}
	return "off"
}

func src(s *thermal.Source) string {
	if s == nil {
		return "-"
	}
	return string(*s)
}

func reasons(rs []draw.Reason) string {
	if len(rs) == 0 {
		return "-"
	}
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return strings.Join(out, ",")
}

func eventText(e engine.Event) string {
	switch {
	case e.Draw != nil:
		d := e.Draw
		return fmt.Sprintf("%s id=%s started=%s litres=%.0f flow=%s kind=%s", e.Type, d.DrawID, ts(d.StartedAt),
			d.LitresSoFar, num(d.FlowLPerMin, 1), d.Kind)
	case e.Ended != nil:
		d := e.Ended
		return fmt.Sprintf("%s id=%s started=%s ended=%s litres=%.0f flow=%s kind=%s seq=%d-%d",
			e.Type, d.DrawID, ts(d.StartedAt), ts(d.EndedAt), d.Litres, num(d.FlowLPerMin, 1), d.Kind, d.From.Seq, d.To.Seq)
	case e.Balanced != nil:
		d := e.Balanced
		shares := make([]string, len(d.Tanks))
		for i, t := range d.Tanks {
			shares[i] = t.DeviceID + ":" + num(t.Share, 2)
		}
		return fmt.Sprintf("%s id=%s ended=%s shares=%s outlet=%s inlet_drop=%s energy=%s hb=%s reliable=%v reasons=%s masked=%v",
			e.Type, d.DrawID, ts(d.EndedAt), strings.Join(shares, ","), num(d.OutletPeakC, 1), num(d.InletDropC, 1),
			num(d.EnergyKWh, 2), num(d.HeatBalanceL, 1), d.HeatBalanceReliable, reasons(d.Reasons), d.MaskedByReheat)
	case e.Flow != nil:
		f := e.Flow
		return fmt.Sprintf("%s since=%s last=%s litres=%.0f flow=%.2f", e.Type, ts(f.Since), ts(f.Last), f.Litres, f.FlowLPerMin)
	case e.Tank != nil:
		t := e.Tank
		return fmt.Sprintf("%s tank=%s %s->%s source=%s power=%s", e.Type, t.DeviceID, t.Prev, t.State, src(t.Source), num(t.PowerKW, 2))
	}
	return string(e.Type)
}

func stateText(s engine.State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "state stored=%s usable_l40=%s usable_l40_low=%s low_hot_water=%s hw=%s ch=%s ambient=%s",
		num(s.StoredKWh, 2), num(s.UsableL40, 1), num(s.UsableL40Low, 1), onOff(s.LowHotWater),
		onOff(s.Boiler.HW), onOff(s.Boiler.CH), num(s.AmbientC, 1))
	if a := s.ActiveDraw; a != nil {
		fmt.Fprintf(&b, " draw=%s:%.0fL", a.DrawID, a.LitresSoFar)
	} else {
		b.WriteString(" draw=-")
	}
	var count *float64
	if s.Meter.Count != nil {
		c := float64(*s.Meter.Count)
		count = &c
	}
	fmt.Fprintf(&b, " meter=%s creep=%v", num(count, 0), s.Meter.ContinuousFlow != nil)
	for _, t := range s.Tanks {
		fmt.Fprintf(&b, " | %s %s src=%s kwh=%s mean=%s top=%s usable=%s power=%s ua=%s",
			t.DeviceID, t.State, src(t.HeatSource), num(t.StoredKWh, 2), num(t.MeanC, 1), num(t.TopC, 1),
			num(t.UsableL40, 1), num(t.PowerKW, 2), num(t.UAWPerK, 1))
	}
	return b.String()
}

// --- JSON

// jnum is a float rounded to 3 decimals in JSON; nil is null.
type jnum float64

func (x jnum) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(float64(x), 'f', -1, 64)), nil
}

func jn(x *float64) *jnum {
	if x == nil {
		return nil
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(*x, 'f', 3, 64), 64)
	v := jnum(r)
	return &v
}

func jv(x float64) *jnum { return jn(&x) }

func jt(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

type eventOut struct {
	Type     string        `json:"type"`
	At       string        `json:"at"`
	Draw     *activeOut    `json:"draw,omitempty"`
	Ended    *endedOut     `json:"draw_ended,omitempty"`
	Balanced *balancedOut  `json:"draw_balanced,omitempty"`
	Flow     *flowOut      `json:"flow,omitempty"`
	Tank     *tankEventOut `json:"tank,omitempty"`
}

type activeOut struct {
	DrawID      string `json:"draw_id"`
	StartedAt   string `json:"started_at"`
	LastLitreAt string `json:"last_litre_at"`
	LitresSoFar *jnum  `json:"litres_so_far"`
	FlowLPerMin *jnum  `json:"flow_l_min"`
	Kind        string `json:"kind"`
}

type refOut struct {
	CountingSince string `json:"counting_since"`
	Seq           uint64 `json:"seq"`
}

// endedOut is draw_ended: the meter's draw. The balance fields are
// absent (draw_balanced carries them), not null.
type endedOut struct {
	DrawID      string `json:"draw_id"`
	StartedAt   string `json:"started_at"`
	EndedAt     string `json:"ended_at"`
	DurationS   *jnum  `json:"duration_s"`
	Litres      *jnum  `json:"litres"`
	VolumeKind  string `json:"volume_kind"`
	FlowLPerMin *jnum  `json:"flow_l_min"`
	Kind        string `json:"kind"`
	MeterFrom   refOut `json:"meter_from"`
	MeterTo     refOut `json:"meter_to"`
}

// balancedOut is draw_balanced: the attribution, with draw_ended's draw_id
// and an identical ended_at.
type balancedOut struct {
	DrawID              string     `json:"draw_id"`
	EndedAt             string     `json:"ended_at"`
	Tanks               []shareOut `json:"tanks"`
	OutletPeakC         *jnum      `json:"outlet_peak_c"`
	InletDropC          *jnum      `json:"inlet_drop_c"`
	EnergyKWh           *jnum      `json:"energy_kwh"`
	HeatBalanceL        *jnum      `json:"heat_balance_l"`
	HeatBalanceReliable bool       `json:"heat_balance_reliable"`
	Reasons             []string   `json:"reasons"`
	MaskedByReheat      bool       `json:"masked_by_reheat"`
}

type shareOut struct {
	DeviceID    string `json:"device_id"`
	Share       *jnum  `json:"share"`
	Litres      *jnum  `json:"litres"`
	OutletPeakC *jnum  `json:"outlet_peak_c"`
	InletDropC  *jnum  `json:"inlet_drop_c"`
}

type flowOut struct {
	Since       string `json:"since"`
	Last        string `json:"last"`
	Litres      *jnum  `json:"litres"`
	FlowLPerMin *jnum  `json:"flow_l_min"`
}

type tankEventOut struct {
	DeviceID string  `json:"device_id"`
	State    string  `json:"state"`
	Prev     string  `json:"prev"`
	Source   *string `json:"source"`
	PowerKW  *jnum   `json:"power_kw"`
}

func srcPtr(s *thermal.Source) *string {
	if s == nil {
		return nil
	}
	x := string(*s)
	return &x
}

func ref(r meter.Ref) refOut {
	return refOut{CountingSince: ts(r.CountingSince), Seq: r.Seq}
}

func activeJSON(d *engine.ActiveDraw) *activeOut {
	if d == nil {
		return nil
	}
	return &activeOut{DrawID: d.DrawID, StartedAt: ts(d.StartedAt), LastLitreAt: ts(d.LastLitreAt),
		LitresSoFar: jv(d.LitresSoFar), FlowLPerMin: jn(d.FlowLPerMin), Kind: string(d.Kind)}
}

func flowJSON(f *engine.Flow) *flowOut {
	if f == nil {
		return nil
	}
	return &flowOut{Since: ts(f.Since), Last: ts(f.Last), Litres: jv(f.Litres), FlowLPerMin: jv(f.FlowLPerMin)}
}

func eventJSON(e engine.Event) eventOut {
	out := eventOut{Type: string(e.Type), At: ts(e.At), Draw: activeJSON(e.Draw), Flow: flowJSON(e.Flow)}
	if d := e.Ended; d != nil {
		out.Ended = &endedOut{DrawID: d.DrawID, StartedAt: ts(d.StartedAt), EndedAt: ts(d.EndedAt), DurationS: jv(d.DurationS),
			Litres: jv(d.Litres), VolumeKind: d.VolumeKind, FlowLPerMin: jn(d.FlowLPerMin), Kind: string(d.Kind),
			MeterFrom: ref(d.From), MeterTo: ref(d.To)}
	}
	if d := e.Balanced; d != nil {
		x := &balancedOut{DrawID: d.DrawID, EndedAt: ts(d.EndedAt),
			OutletPeakC: jn(d.OutletPeakC), InletDropC: jn(d.InletDropC), EnergyKWh: jn(d.EnergyKWh),
			HeatBalanceL: jn(d.HeatBalanceL), HeatBalanceReliable: d.HeatBalanceReliable, Reasons: []string{},
			MaskedByReheat: d.MaskedByReheat}
		for _, t := range d.Tanks {
			x.Tanks = append(x.Tanks, shareOut{DeviceID: t.DeviceID, Share: jn(t.Share), Litres: jn(t.Litres),
				OutletPeakC: jn(t.OutletPeakC), InletDropC: jn(t.InletDropC)})
		}
		for _, r := range d.Reasons {
			x.Reasons = append(x.Reasons, string(r))
		}
		out.Balanced = x
	}
	if t := e.Tank; t != nil {
		out.Tank = &tankEventOut{DeviceID: t.DeviceID, State: string(t.State), Prev: string(t.Prev),
			Source: srcPtr(t.Source), PowerKW: jn(t.PowerKW)}
	}
	return out
}

type stateOut struct {
	Type         string      `json:"type"`
	At           string      `json:"at"`
	StoredKWh    *jnum       `json:"stored_kwh"`
	UsableL40    *jnum       `json:"usable_l40"`
	UsableL40Low *jnum       `json:"usable_l40_low"`
	LowHotWater  *bool       `json:"low_hot_water"`
	Boiler       boilerOut   `json:"boiler"`
	AmbientC     *jnum       `json:"ambient_c"`
	ActiveDraw   *activeOut  `json:"active_draw"`
	Meter        meterOut    `json:"meter"`
	Tanktemp     tanktempOut `json:"tanktemp"`
	Tanks        []tankOut   `json:"tanks"`
}

type boilerOut struct {
	HW *bool `json:"hw"`
	CH *bool `json:"ch"`
}

type meterOut struct {
	At             *string  `json:"at"`
	Count          *uint64  `json:"count"`
	CountingSince  *string  `json:"counting_since"`
	Litres         *jnum    `json:"litres"`
	ContinuousFlow *flowOut `json:"continuous_flow"`
}

type tanktempOut struct {
	At      *string `json:"at"`
	Online  *bool   `json:"online"`
	Version *string `json:"version"`
}

type tankOut struct {
	DeviceID     string  `json:"device_id"`
	At           *string `json:"at"`
	State        string  `json:"state"`
	HeatSource   *string `json:"heat_source"`
	StoredKWh    *jnum   `json:"stored_kwh"`
	MeanC        *jnum   `json:"mean_c"`
	TopC         *jnum   `json:"top_c"`
	HotL         *jnum   `json:"hot_l"`
	UsableL40    *jnum   `json:"usable_l40"`
	UsableL40Low *jnum   `json:"usable_l40_low"`
	PowerKW      *jnum   `json:"power_kw"`
	UAWPerK      *jnum   `json:"ua_w_per_k"`
	ProbesOK     *int    `json:"probes_ok"`
	ProbesTotal  *int    `json:"probes_total"`
}

func stateJSON(s engine.State) stateOut {
	out := stateOut{Type: "state", At: ts(s.At), StoredKWh: jn(s.StoredKWh), UsableL40: jn(s.UsableL40),
		UsableL40Low: jn(s.UsableL40Low), LowHotWater: s.LowHotWater, Boiler: boilerOut{HW: s.Boiler.HW, CH: s.Boiler.CH},
		AmbientC: jn(s.AmbientC), ActiveDraw: activeJSON(s.ActiveDraw),
		Meter: meterOut{At: jt(s.Meter.At), Count: s.Meter.Count, CountingSince: jt(s.Meter.CountingSince),
			Litres: jn(s.Meter.Litres), ContinuousFlow: flowJSON(s.Meter.ContinuousFlow)},
		Tanktemp: tanktempOut{At: jt(s.Tanktemp.At), Online: s.Tanktemp.Online, Version: s.Tanktemp.Version},
	}
	for _, t := range s.Tanks {
		out.Tanks = append(out.Tanks, tankOut{DeviceID: t.DeviceID, At: jt(t.At), State: string(t.State),
			HeatSource: srcPtr(t.HeatSource), StoredKWh: jn(t.StoredKWh), MeanC: jn(t.MeanC), TopC: jn(t.TopC),
			HotL: jn(t.HotL), UsableL40: jn(t.UsableL40), UsableL40Low: jn(t.UsableL40Low), PowerKW: jn(t.PowerKW),
			UAWPerK: jn(t.UAWPerK), ProbesOK: t.ProbesOK, ProbesTotal: t.ProbesTotal})
	}
	return out
}
