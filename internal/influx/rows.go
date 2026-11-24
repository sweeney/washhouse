package influx

import (
	"sort"
	"strings"
)

// Rows writes each retained series' row once, with its final content.
// Influx merges a second write at the same (measurement, tags, time):
// the fields it carries replace the old, and the fields it leaves out are
// kept. washhouse publishes a topic more than once at one timestamp (both
// tanks' sweeps share one, WIRE.md), and a field that went unknown is left
// out, so Influx would keep the stale value (tanktemp's review of #8). So
// the tank and system series hold their latest point until a later time
// arrives, and Flush writes what is held at shutdown: a row is written
// one publish late, and a crash loses at most the held one per series.
// The events' measurements pass straight through: a draw's two events
// merge into one row on purpose, and no field of theirs goes unknown.
type Rows struct{ held map[string]Point }

// heldMeasurements are the series written from retained messages.
var heldMeasurements = map[string]bool{"hot_water_tank": true, "hot_water_system": true}

// Add is what to write now that p is published.
func (r *Rows) Add(p Point) []Point {
	if !heldMeasurements[p.Measurement] {
		return []Point{p}
	}
	if r.held == nil {
		r.held = map[string]Point{}
	}
	key := series(p)
	was, ok := r.held[key]
	switch {
	case !ok || was.Time.Equal(p.Time):
		r.held[key] = p
		return nil
	case p.Time.Before(was.Time): // out of order: not this row
		return []Point{p}
	}
	r.held[key] = p
	return []Point{was}
}

// Flush is what is held, in series order, to be written at shutdown.
func (r *Rows) Flush() []Point {
	keys := make([]string, 0, len(r.held))
	for k := range r.held {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		out = append(out, r.held[k])
	}
	r.held = nil
	return out
}

// series is a point's series: its measurement and sorted tags.
func series(p Point) string {
	tags := make([]string, 0, len(p.Tags))
	for k, v := range p.Tags {
		tags = append(tags, k+"="+v)
	}
	sort.Strings(tags)
	return strings.Join(append([]string{p.Measurement}, tags...), ",")
}
