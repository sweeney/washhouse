package meter

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The recorded fixtures (testdata/fixtures, real meter data shifted to a fake
// epoch) replayed through the consumer, against tools/meter_audit.py's reading
// of the same files in draws.json (regenerate with tools/fixture_draws.py).
//
// They are meant to agree on every draw of two litres or more. A one-litre
// audit "draw" is either a one-litre draw here or a creep litre: the audit has
// no creep rule. The audit can also count a litre from the retained message at
// the start of a recording, which the consumer takes as its baseline.

const fixtureDir = "../../testdata/fixtures"

type auditFixture struct {
	Litres int `json:"litres"`
	Draws  []struct {
		Start      time.Time `json:"start"`
		Litres     int       `json:"litres"`
		DurationS  float64   `json:"duration_s"`
		MedianFlow *float64  `json:"median_flow_l_min"`
	} `json:"draws"`
}

func loadAuditFixtures(t *testing.T) map[string]auditFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, "draws.json"))
	if err != nil {
		t.Fatalf("%v (run python3 tools/fixture_draws.py)", err)
	}
	var m map[string]auditFixture
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) == 0 {
		t.Fatal("draws.json lists no fixtures")
	}
	return m
}

type replayed struct {
	draws []Draw
	creep int // creep litres (litres in no draw)
	flows int // continuous_flow_started events
}

func replayFixture(t *testing.T, name string) replayed {
	t.Helper()
	c := newConsumer(t, planConfig())
	var r replayed
	inDraw := 0
	for _, e := range replay(t, c, filepath.Join(fixtureDir, name+".log.gz")) {
		switch e.Type {
		case EventDrawEnded:
			r.draws = append(r.draws, *e.Draw)
			inDraw += e.Draw.Pulses
		case EventContinuousFlowStarted:
			r.flows++
		}
	}
	total := 0
	for _, e := range replay(t, newConsumer(t, planConfig()), filepath.Join(fixtureDir, name+".log.gz")) {
		if e.Type == EventLitre {
			total += e.Litre.Pulses
		}
	}
	r.creep = total - inDraw
	return r
}

func TestFixturesMatchTheAudit(t *testing.T) {
	for name, want := range loadAuditFixtures(t) {
		t.Run(name, func(t *testing.T) {
			got := replayFixture(t, name)
			var multi []Draw
			for _, d := range got.draws {
				if d.Pulses >= 2 {
					multi = append(multi, d)
				}
			}
			i := 0
			ones := 0
			for _, w := range want.Draws {
				if w.Litres < 2 {
					ones++
					continue
				}
				if i >= len(multi) {
					t.Errorf("audit draw at %s (%d L) missing", w.Start.Format(time.TimeOnly), w.Litres)
					continue
				}
				g := multi[i]
				i++
				if !g.Start.Equal(w.Start) || g.Pulses != w.Litres || math.Abs(g.Duration.Seconds()-w.DurationS) > 0.05 {
					t.Errorf("draw at %s: got %d L over %.1fs from %s, audit %d L over %.1fs",
						w.Start.Format(time.TimeOnly), g.Pulses, g.Duration.Seconds(), g.Start.Format(time.TimeOnly), w.Litres, w.DurationS)
				}
				if w.MedianFlow != nil && (g.FlowLPerMin == nil || math.Abs(*g.FlowLPerMin-*w.MedianFlow) > 0.05) {
					t.Errorf("draw at %s: flow %v, audit %v", w.Start.Format(time.TimeOnly), g.FlowLPerMin, *w.MedianFlow)
				}
			}
			if i != len(multi) {
				t.Errorf("%d extra multi-litre draws", len(multi)-i)
			}
			// Every audit one-litre draw is a one-litre draw or a creep litre
			// here, give or take the audit's baseline litre.
			single := len(got.draws) - len(multi)
			if d := ones - (single + got.creep); d < 0 || d > 1 {
				t.Errorf("audit one-litre draws %d, here %d one-litre draws + %d creep litres", ones, single, got.creep)
			}
		})
	}
}

// What each fixture is known to show (testdata/fixtures/README.md).
func TestFixtureFacts(t *testing.T) {
	kinds := func(r replayed) map[Kind][]int {
		m := map[Kind][]int{}
		for _, d := range r.draws {
			m[d.Kind] = append(m[d.Kind], d.Pulses)
		}
		return m
	}
	t.Run("shower-layered", func(t *testing.T) {
		k := kinds(replayFixture(t, "shower-layered"))
		if len(k[KindShower]) != 1 || k[KindShower][0] != 50 {
			t.Errorf("showers %v, want one of 50 L", k[KindShower])
		}
	})
	for name, litres := range map[string]int{"bath-in-reheat": 72, "bath-87": 87} {
		t.Run(name, func(t *testing.T) {
			k := kinds(replayFixture(t, name))
			if len(k[KindBath]) != 1 || k[KindBath][0] != litres {
				t.Errorf("baths %v, want one of %d L", k[KindBath], litres)
			}
		})
	}
	t.Run("creep-evening", func(t *testing.T) {
		if r := replayFixture(t, "creep-evening"); r.flows == 0 || r.creep == 0 {
			t.Errorf("continuous flows %d, creep litres %d: want both", r.flows, r.creep)
		}
	})
	t.Run("quiet-night", func(t *testing.T) {
		if r := replayFixture(t, "quiet-night"); len(r.draws) != 0 || r.creep != 0 {
			t.Errorf("draws %d, creep litres %d: want none", len(r.draws), r.creep)
		}
	})
	t.Run("taps-small", func(t *testing.T) {
		for _, d := range replayFixture(t, "taps-small").draws {
			if d.Kind != KindTap {
				t.Errorf("%d L draw is %s, want tap", d.Pulses, d.Kind)
			}
		}
	})
}
