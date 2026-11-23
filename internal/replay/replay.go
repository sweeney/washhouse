// Package replay reads recordings in mosquitto_sub's `%I %t %p` format
// (receive time, topic, payload; gzipped or not) into decoded messages, in
// the order they were received. It is how tests, and cmd/washreplay, feed
// recorded fixtures through washhouse.
//
// It knows washhouse's inputs: tanktemp's state, status and meter (under
// water/hot/tanktemp/ or the older energy/hot_water/), the boiler
// programmer's events and system topics, and the boiler-room climate
// sensor's topic, which is configured (its payload has no timestamp, so its
// time is the receive time). Other topics are skipped.
package replay

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sweeney/washhouse/internal/ingest"
)

// Options say which topics to read beyond the fixed ones.
type Options struct {
	Ambient string // the boiler-room climate topic; "" skips it
}

// Msg is one decoded message, as ingest decodes it.
type Msg = ingest.Msg

// Ambient is one boiler-room temperature reading.
type Ambient = ingest.Ambient

// sources are the topics a recording may hold: tanktemp under both its
// prefixes, since recordings made before the move have the old one.
func sources(o Options) ingest.Sources {
	return ingest.Sources{Tanktemp: []string{"water/hot/tanktemp", "energy/hot_water"},
		Boiler: "energy/boiler/sensor", Ambient: o.Ambient}
}

// ReadFile reads a recording, gunzipping it if its name ends in .gz.
func ReadFile(path string, o Options) ([]Msg, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		z, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defer z.Close()
		r = z
	}
	msgs, err := Read(r, o)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return msgs, nil
}

// Read decodes every line it knows. A line it should know but can't decode
// is an error, naming the line.
func Read(r io.Reader, o Options) ([]Msg, error) {
	var out []Msg
	src := sources(o)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 3 {
			return nil, fmt.Errorf("line %d: not `time topic payload`", n)
		}
		rx, err := time.Parse("2006-01-02T15:04:05-0700", parts[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: receive time: %w", n, err)
		}
		m, ok, err := src.Decode(parts[1], []byte(parts[2]), rx.UTC())
		if err != nil {
			return nil, fmt.Errorf("line %d (%s): %w", n, parts[1], err)
		}
		if ok {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}
