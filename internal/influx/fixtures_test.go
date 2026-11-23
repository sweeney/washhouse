package influx

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/behaviour"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/ingest"
	"github.com/sweeney/washhouse/internal/publish"
	"github.com/sweeney/washhouse/internal/replay"
)

// Over every fixture, as the service publishes it: every message maps;
// no field changes type within a measurement (Influx refuses the batch
// that does, statehouse's review); every point has a field; and each
// draw's draw_balanced lands on its draw_ended's row (same tags, same
// time), never on a row of its own.
func TestPointsOverTheFixtures(t *testing.T) {
	f, err := os.Open("../../testdata/behaviour.example.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bc, ec, err := behaviour.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob("../../testdata/fixtures/*.log.gz")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	m := Mapper{Base: "water/hot", Site: "home"}
	types := map[string]string{} // measurement.field → its Go type
	measurements := map[string]int{}
	balanced := 0
	for _, path := range paths {
		eng, err := engine.New(ec)
		if err != nil {
			t.Fatal(err)
		}
		pub, err := publish.New(m.Base)
		if err != nil {
			t.Fatal(err)
		}
		msgs, err := replay.ReadFile(path, replay.Options{Ambient: bc.AmbientTopic})
		if err != nil {
			t.Fatal(err)
		}
		rows := map[string]time.Time{} // draw_id → its draw_ended row's time
		after := func(ev []engine.Event) {
			out, err := pub.After(eng.State(), ev)
			if err != nil {
				t.Fatal(err)
			}
			for _, msg := range out {
				ps, err := m.Points(msg)
				if err != nil {
					t.Fatalf("%s: %v", filepath.Base(path), err)
				}
				for _, p := range ps {
					measurements[p.Measurement]++
					if len(p.Fields) == 0 {
						t.Errorf("%s: a %s point with no fields", filepath.Base(path), p.Measurement)
					}
					for k, v := range p.Fields {
						typ := fmt.Sprintf("%T", v)
						if typ != "float64" && typ != "int64" && typ != "string" {
							t.Errorf("%s.%s is a %s", p.Measurement, k, typ)
						}
						key := p.Measurement + "." + k
						if was, ok := types[key]; ok && was != typ {
							t.Errorf("%s: %s was a %s, now a %s", filepath.Base(path), key, was, typ)
						}
						types[key] = typ
					}
					if p.Measurement != "hot_water_draw" {
						continue
					}
					id := p.Fields["draw_id"].(string)
					if len(p.Tags) != 1 || p.Tags["site"] != "home" {
						t.Errorf("draw %s tags %v", id, p.Tags)
					}
					if _, ok := p.Fields["litres"]; ok { // draw_ended
						rows[id] = p.Time
						continue
					}
					balanced++
					if row, ok := rows[id]; !ok || !row.Equal(p.Time) {
						t.Errorf("%s: draw %s balanced at %v, its row is at %v (%v)", filepath.Base(path), id, p.Time, row, ok)
					}
				}
			}
		}
		for _, msg := range msgs {
			ev, _ := ingest.Feed[engine.Event](eng, msg)
			after(ev)
		}
		if eng.Pending() {
			after(eng.Tick(eng.State().At.Add(eng.Settle())))
		}
	}
	for _, name := range []string{"hot_water_tank", "hot_water_system", "hot_water_draw", "hot_water_draw_tank", "hot_water_continuous_flow"} {
		if measurements[name] == 0 {
			t.Errorf("no fixture wrote %s", name)
		}
	}
	if balanced == 0 {
		t.Error("no draw was balanced")
	}
	t.Logf("points per measurement %v; %d draws balanced", measurements, balanced)
}
