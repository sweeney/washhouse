package httpapi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sweeney/washhouse/internal/behaviour"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/ingest"
	"github.com/sweeney/washhouse/internal/replay"
)

const behaviourExample = "../../testdata/behaviour.example.json"

func fixtures(t *testing.T) []string {
	t.Helper()
	ps, err := filepath.Glob("../../testdata/fixtures/*.log.gz")
	if err != nil || len(ps) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	return ps
}

func exampleBehaviour(t *testing.T) (behaviour.Config, engine.Config, []byte) {
	t.Helper()
	b, err := os.ReadFile(behaviourExample)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(behaviourExample)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bc, ec, err := behaviour.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	return bc, ec, b
}

// replayInto runs a recording through an engine as the service does, each
// input's state and events going to rec (and to each, when not nil), and
// at the end a silence long enough for a draw still pending to settle.
func replayInto(t *testing.T, path string, rec *Recorder, each func(*Snapshot)) {
	t.Helper()
	bc, ec, _ := exampleBehaviour(t)
	eng, err := engine.New(ec)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := replay.ReadFile(path, replay.Options{Ambient: bc.AmbientTopic})
	if err != nil {
		t.Fatal(err)
	}
	after := func(ev []engine.Event) {
		if err := rec.After(eng.State(), ev); err != nil {
			t.Fatal(err)
		}
		if each != nil {
			each(rec.Snapshot())
		}
	}
	for _, m := range msgs {
		ev, _ := ingest.Feed[engine.Event](eng, m)
		after(ev)
	}
	if eng.Pending() {
		after(eng.Tick(eng.State().At.Add(eng.Settle())))
	}
}
