package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sweeney/washhouse/internal/wire/washhouse"
)

// -wire prints what the service would publish, one `topic payload` line
// per message, and pins it for the layered shower.
func TestGoldenWire(t *testing.T) {
	golden(t, "shower-layered.wire", runOK(t, "-config", examplePath, "-wire", fixture))
}

// Every fixture through the engine and the publisher: every message
// decodes under its topic, and the contract's rules across messages hold
// (docs/WIRE.md).
func TestWireContractOnFixtures(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/fixtures/*.log.gz")
	if len(files) < 10 {
		t.Fatalf("%d fixtures", len(files))
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			out := runOK(t, "-config", examplePath, "-wire", f)
			jsonEvents := 0
			for _, l := range strings.Split(runOK(t, "-config", examplePath, "-state-every", "0", "-json", f), "\n") {
				if l != "" && !strings.HasPrefix(l, `{"type":"state"`) {
					jsonEvents++
				}
			}
			checkWire(t, out, jsonEvents)
		})
	}
}

func checkWire(t *testing.T, out string, wantEvents int) {
	t.Helper()
	const base = "water/hot/"
	last := map[string]string{}
	started := map[string]bool{}
	endedAt := map[string]washhouse.Time{}
	balanced := map[string]bool{}
	justEnded := ""
	events, states := 0, 0
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		topic, payload, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("line %d isn't `topic payload`: %q", i+1, line)
		}
		p := []byte(payload)
		if topic != base+"events" {
			if last[topic] == payload {
				t.Errorf("line %d: %s published twice unchanged", i+1, topic)
			}
			last[topic] = payload
		}
		switch {
		case topic == base+"state":
			s, err := washhouse.DecodeState(p)
			if err != nil {
				t.Fatalf("line %d: %v", i+1, err)
			}
			states++
			if justEnded != "" && s.ActiveDraw != nil && s.ActiveDraw.DrawID == justEnded {
				t.Errorf("line %d: the state after %s's draw_ended still has it", i+1, justEnded)
			}
			justEnded = ""
		case strings.HasPrefix(topic, base+"tank/"):
			tk, err := washhouse.DecodeTank(p)
			if err != nil {
				t.Fatalf("line %d: %v", i+1, err)
			}
			if topic != washhouse.TankTopic("water/hot", tk.DeviceID) {
				t.Errorf("line %d: %s on %s", i+1, tk.DeviceID, topic)
			}
			if tk.SweepAt != nil && tk.SweepAt.After(tk.Timestamp.Time) {
				t.Errorf("line %d: sweep_at %v after timestamp %v", i+1, tk.SweepAt, tk.Timestamp)
			}
		case topic == base+"events":
			e, err := washhouse.DecodeEvent(p)
			if err != nil {
				t.Fatalf("line %d: %v", i+1, err)
			}
			events++
			switch e.Event {
			case washhouse.EventDrawStarted:
				started[e.ActiveDraw.DrawID] = true
			case washhouse.EventDrawUpdated:
				if !started[e.ActiveDraw.DrawID] {
					t.Errorf("line %d: draw_updated %s before its draw_started", i+1, e.ActiveDraw.DrawID)
				}
			case washhouse.EventDrawEnded:
				d := e.DrawEnded
				endedAt[d.DrawID] = d.EndedAt
				justEnded = d.DrawID
				if !e.Timestamp.After(d.EndedAt.Time) {
					t.Errorf("line %d: draw_ended at %v, not after the last litre %v", i+1, e.Timestamp, d.EndedAt)
				}
			case washhouse.EventDrawBalanced:
				d := e.DrawBalanced
				ea, ok := endedAt[d.DrawID]
				if !ok || !ea.Equal(d.EndedAt.Time) {
					t.Errorf("line %d: draw_balanced %s ended_at %v, its draw_ended's %v (%v)", i+1, d.DrawID, d.EndedAt, ea, ok)
				}
				balanced[d.DrawID] = true
				masked := false
				for _, r := range d.Reasons {
					masked = masked || r == washhouse.ReasonHWOn || r == washhouse.ReasonGained || r == washhouse.ReasonHWUnknown
				}
				for _, tk := range d.Tanks {
					if masked && tk.Share != nil {
						t.Errorf("line %d: masked draw %s has a share for %s", i+1, d.DrawID, tk.DeviceID)
					}
				}
			}
		default:
			t.Errorf("line %d: unexpected topic %s", i+1, topic)
		}
	}
	if events != wantEvents {
		t.Errorf("%d events on the wire, %d from the engine", events, wantEvents)
	}
	if states == 0 {
		t.Error("no state published")
	}
	for id := range endedAt {
		if !balanced[id] {
			t.Errorf("draw %s ended but never balanced", id)
		}
	}
	if bytes.Contains([]byte(out), []byte("/tanktemp/")) {
		t.Error("published inside tanktemp's subtree")
	}
}
