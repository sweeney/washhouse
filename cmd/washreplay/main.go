// Command washreplay replays recordings of washhouse's inputs through the
// engine and prints what it decides: its events, one per line, and the
// system's state every so often (in event time).
//
//	washreplay -config FILE [-state-every 10m] [-json] RECORDING...
//	washreplay -config FILE -wire [-base water/hot] RECORDING...
//
// -wire prints instead every message the service would publish
// (docs/WIRE.md), as `topic payload` lines, the way mosquitto_sub -F
// '%t %p' prints them: the retained state and tank messages when their
// content changes, and each event. -state-every doesn't apply to it.
//
// Recordings are mosquitto_sub `-F '%I %t %p'` logs, gzipped (.gz) or
// plain, as internal/replay reads them; several are fed in order through
// one engine. The engine's config comes from -config, a JSON file
// (testdata/behaviour.example.json has the plan's values); nothing
// about the cylinders or the thresholds is built in.
//
// After the last recording, if a draw is still running or waiting for its
// attribution, event time is moved on far enough for it to end, so every
// draw gets its draw_ended and its draw_balanced. The last state line is
// printed before that.
//
// It is the tuning and test tool PLAN.md describes, not the service: it
// reads files and writes to stdout.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sweeney/washhouse/internal/behaviour"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/ingest"
	"github.com/sweeney/washhouse/internal/publish"
	"github.com/sweeney/washhouse/internal/replay"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("washreplay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: washreplay -config FILE [-state-every 10m] [-json | -wire [-base water/hot]] FILE...")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "the engine's config, JSON (required)")
	every := fs.Duration("state-every", 10*time.Minute, "print the state this often in event time (0: only at the end)")
	asJSON := fs.Bool("json", false, "print JSON lines")
	asWire := fs.Bool("wire", false, "print what the service would publish, as `topic payload` lines")
	base := fs.String("base", "water/hot", "the base topic, with -wire")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch {
	case *configPath == "":
		fmt.Fprintln(stderr, "washreplay: -config is required")
		fs.Usage()
		return 2
	case fs.NArg() == 0:
		fmt.Fprintln(stderr, "washreplay: no recording given (FILE...)")
		fs.Usage()
		return 2
	case *asJSON && *asWire:
		fmt.Fprintln(stderr, "washreplay: -json and -wire can't be used together")
		return 2
	case *every < 0:
		fmt.Fprintln(stderr, "washreplay: -state-every must be at least 0")
		return 2
	}
	fc, cfg, err := readConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "washreplay: %s: %v\n", *configPath, err)
		return 1
	}
	e, err := engine.New(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "washreplay: %v\n", err)
		return 1
	}
	w := bufio.NewWriter(stdout)
	defer w.Flush()
	p := &printer{w: w, json: *asJSON, every: *every}
	if *asWire {
		if p.pub, err = publish.New(*base); err != nil {
			fmt.Fprintf(stderr, "washreplay: %v\n", err)
			return 2
		}
	}
	for _, path := range fs.Args() {
		msgs, err := replay.ReadFile(path, replay.Options{Ambient: fc.AmbientTopic})
		if err != nil {
			fmt.Fprintf(stderr, "washreplay: %v\n", err)
			return 1
		}
		for _, m := range msgs {
			ev, err := feed(e, m)
			if err != nil {
				fmt.Fprintf(stderr, "washreplay: %s %s: %v\n", m.Received.Format(time.RFC3339), m.Topic, err)
			}
			if p.pub != nil {
				p.wire(e.State(), ev)
				continue
			}
			p.events(ev)
			p.maybeState(e.State())
		}
	}
	s := e.State()
	if p.pub != nil {
		if e.Pending() {
			ev := e.Tick(s.At.Add(e.Settle()))
			p.wire(e.State(), ev)
		}
	} else {
		p.state(s)
		if e.Pending() {
			p.events(e.Tick(s.At.Add(e.Settle())))
		}
	}
	if p.err != nil {
		fmt.Fprintf(stderr, "washreplay: %v\n", p.err)
		return 1
	}
	return 0
}

func readConfig(path string) (behaviour.Config, engine.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return behaviour.Config{}, engine.Config{}, err
	}
	defer f.Close()
	return behaviour.Load(f)
}

// feed gives one recorded message to the engine.
func feed(e *engine.Engine, m replay.Msg) ([]engine.Event, error) {
	return ingest.Feed[engine.Event](e, m)
}
