package influx

import (
	"reflect"
	"testing"
	"time"
)

// A tank's or the system's row is written once, with the last content at
// its time; a later time releases it; the events pass through; Flush
// gives up what is held, in a fixed order, once.
func TestRows(t *testing.T) {
	t1 := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Second)
	tank := func(id string, at time.Time, f map[string]any) Point {
		return Point{"hot_water_tank", map[string]string{"site": "home", "device_id": id}, f, at}
	}
	var r Rows
	if got := r.Add(tank("left", t1, map[string]any{"state": "heating", "heat_source": "hw"})); len(got) != 0 {
		t.Errorf("the first: %v", got)
	}
	final := tank("left", t1, map[string]any{"state": "cooling"})
	if got := r.Add(final); len(got) != 0 {
		t.Errorf("the same time again: %v", got)
	}
	right := tank("right", t1, map[string]any{"state": "cooling"})
	if got := r.Add(right); len(got) != 0 {
		t.Errorf("another series: %v", got)
	}
	next := tank("left", t2, map[string]any{"state": "cooling"})
	if got := r.Add(next); !reflect.DeepEqual(got, []Point{final}) {
		t.Errorf("a later time: %v, want the held row's last content", got)
	}
	// An earlier time than the held row's is written as it comes.
	late := tank("left", t1.Add(-time.Second), map[string]any{"state": "cooling"})
	if got := r.Add(late); !reflect.DeepEqual(got, []Point{late}) {
		t.Errorf("an earlier time: %v", got)
	}
	sys := Point{"hot_water_system", map[string]string{"site": "home"}, map[string]any{"drawing": int64(0)}, t1}
	if got := r.Add(sys); len(got) != 0 {
		t.Errorf("the system: %v", got)
	}
	for _, m := range []string{"hot_water_draw", "hot_water_draw_tank", "hot_water_continuous_flow"} {
		p := Point{m, map[string]string{"site": "home"}, map[string]any{"draw_id": "x"}, t1}
		if got := r.Add(p); !reflect.DeepEqual(got, []Point{p}) {
			t.Errorf("%s: %v, want it at once", m, got)
		}
	}
	if got := r.Flush(); !reflect.DeepEqual(got, []Point{sys, next, right}) {
		t.Errorf("flush: %v", got)
	}
	if got := r.Flush(); len(got) != 0 {
		t.Errorf("a second flush: %v", got)
	}
}
