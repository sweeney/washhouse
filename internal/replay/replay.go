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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sweeney/washhouse/internal/wire/boiler"
	"github.com/sweeney/washhouse/internal/wire/tanktemp"
)

// Options say which topics to read beyond the fixed ones.
type Options struct {
	Ambient string // the boiler-room climate topic; "" skips it
}

// Ambient is one boiler-room temperature reading.
type Ambient struct {
	At    time.Time // the receive time
	TempC float64
}

// Msg is one decoded message. Exactly one of the pointers is set.
type Msg struct {
	Received time.Time
	Topic    string

	State        *tanktemp.State
	Status       *tanktemp.Status
	Meter        *tanktemp.Meter
	BoilerEvent  *boiler.Event
	BoilerSystem *boiler.System
	Ambient      *Ambient
}

const (
	boilerEvents = "energy/boiler/sensor/events"
	boilerSystem = "energy/boiler/sensor/system"
)

var tanktempPrefixes = []string{"water/hot/tanktemp/", "energy/hot_water/"}

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
		m := Msg{Received: rx.UTC(), Topic: parts[1]}
		ok, err := decode(&m, []byte(parts[2]), o)
		if err != nil {
			return nil, fmt.Errorf("line %d (%s): %w", n, m.Topic, err)
		}
		if ok {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

func decode(m *Msg, b []byte, o Options) (bool, error) {
	switch m.Topic {
	case boilerEvents:
		e, err := boiler.DecodeEvent(b)
		m.BoilerEvent = &e
		return true, err
	case boilerSystem:
		s, err := boiler.DecodeSystem(b)
		m.BoilerSystem = &s
		return true, err
	}
	if o.Ambient != "" && m.Topic == o.Ambient {
		var c struct {
			Temperature *float64 `json:"temperature"`
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return true, err
		}
		if c.Temperature == nil {
			return false, nil // a climate message without a temperature: nothing to use
		}
		m.Ambient = &Ambient{At: m.Received, TempC: *c.Temperature}
		return true, nil
	}
	for _, p := range tanktempPrefixes {
		rest, ok := strings.CutPrefix(m.Topic, p)
		if !ok {
			continue
		}
		switch {
		case rest == "meter":
			x, err := tanktemp.DecodeMeter(b)
			m.Meter = &x
			return true, err
		case rest == "status":
			x, err := tanktemp.DecodeStatus(b)
			m.Status = &x
			return true, err
		case strings.HasSuffix(rest, "/state") && !strings.Contains(strings.TrimSuffix(rest, "/state"), "/"):
			x, err := tanktemp.DecodeState(b)
			m.State = &x
			return true, err
		}
		return false, nil
	}
	return false, nil
}
