package meter

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auditDraw is one draw as tools/meter_audit.py reports it.
type auditDraw struct {
	Start      string   `json:"start"`
	End        string   `json:"end"`
	Litres     int      `json:"litres"`
	DurationS  float64  `json:"duration_s"`
	MedianFlow *float64 `json:"median_flow_l_min"`
}

// TestAuditParity replays each scenario in testdata/audit (written by
// tools/meter_scenarios.py from meter_audit_test.py's emitter) and checks
// the Go finds the draws meter_audit.py found: the same litres, start, end,
// duration and median flow (the audit rounds both to 0.1 with Python's
// round, half to even).
func TestAuditParity(t *testing.T) {
	logs, err := filepath.Glob(filepath.Join("testdata", "audit", "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) < 8 {
		t.Fatalf("only %d scenarios: run python3 tools/meter_scenarios.py", len(logs))
	}
	for _, log := range logs {
		name := strings.TrimSuffix(filepath.Base(log), ".log")
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(strings.TrimSuffix(log, ".log") + ".draws.json")
			if err != nil {
				t.Fatal(err)
			}
			var want []auditDraw
			if err := json.Unmarshal(b, &want); err != nil {
				t.Fatal(err)
			}
			got := endedDraws(replay(t, newConsumer(t, planConfig()), log))
			if len(got) != len(want) {
				t.Fatalf("%d draws, audit has %d: %+v", len(got), len(want), got)
			}
			for i, w := range want {
				g := got[i]
				if g.Litres != float64(w.Litres) {
					t.Errorf("draw %d: %v L, audit %d", i, g.Litres, w.Litres)
				}
				if s := g.Start.Format("2006-01-02T15:04:05.000Z"); s != w.Start {
					t.Errorf("draw %d: start %s, audit %s", i, s, w.Start)
				}
				if e := g.End.Format("2006-01-02T15:04:05.000Z"); e != w.End {
					t.Errorf("draw %d: end %s, audit %s", i, e, w.End)
				}
				if d := math.RoundToEven(g.Duration.Seconds()*10) / 10; d != w.DurationS {
					t.Errorf("draw %d: duration %v s, audit %v", i, d, w.DurationS)
				}
				switch {
				case w.MedianFlow == nil && g.FlowLPerMin != nil:
					t.Errorf("draw %d: flow %v, audit none", i, *g.FlowLPerMin)
				case w.MedianFlow != nil && g.FlowLPerMin == nil:
					t.Errorf("draw %d: no flow, audit %v", i, *w.MedianFlow)
				case w.MedianFlow != nil && math.RoundToEven(*g.FlowLPerMin*10)/10 != *w.MedianFlow:
					t.Errorf("draw %d: flow %v, audit %v", i, *g.FlowLPerMin, *w.MedianFlow)
				}
			}
		})
	}
}

// The kinds scenario pins the kind rule on the audit's draws.
func TestAuditKinds(t *testing.T) {
	got := endedDraws(replay(t, newConsumer(t, planConfig()), filepath.Join("testdata", "audit", "kinds.log")))
	want := []Kind{KindShower, KindBath, KindTap, KindTap, KindShower, KindTap}
	if len(got) != len(want) {
		t.Fatalf("%d draws, want %d", len(got), len(want))
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Errorf("draw %d (%v L, %v, %v L/min): %s, want %s", i, got[i].Litres, got[i].Duration, *got[i].FlowLPerMin, got[i].Kind, k)
		}
	}
}
