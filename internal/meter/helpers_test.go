package meter

import (
	"bufio"
	"compress/gzip"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/washhouse/internal/wire/tanktemp"
)

// planConfig is PLAN.md's thresholds.
func planConfig() Config {
	return Config{
		DeviceID:             "hot_water_meter",
		DrawEnd:              60 * time.Second,
		BathFlowLPerMin:      10,
		BathMinLitres:        20,
		ShowerMinLitres:      20,
		ShowerMinDuration:    180 * time.Second,
		ShowerMinFlowLPerMin: 3,
		CreepClosed:          10 * time.Second,
		CreepWindow:          2700 * time.Second,
		StaleAfter:           90 * time.Second,
	}
}

func newConsumer(t *testing.T, cfg Config) *Consumer {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var t0 = time.Date(2001, 1, 1, 7, 0, 0, 0, time.UTC)

// sim emits meter messages the way tanktemp's WIRE.md says the meter
// publishes them, numbering seq at publish: a Go port of
// tools/meter_audit_test.py's Meter, with the pulse and the open separable.
type sim struct {
	since        time.Time
	k            float64
	seq, count   uint64
	level        tanktemp.Level
	lastPulse    *time.Time
	lastInterval *int64
	lastClosed   *int64
	lost         uint64
	unsynced     bool // stamp clock_synced false
}

func newSim(since time.Time) *sim {
	return &sim{since: since, k: 1, level: tanktemp.LevelOpen}
}

func (s *sim) emit(tr tanktemp.Trigger, at time.Time) tanktemp.Meter {
	s.seq++
	m := tanktemp.Meter{
		DeviceID: "hot_water_meter", Timestamp: at.Truncate(time.Second), ClockSynced: !s.unsynced,
		Trigger: tr, Seq: s.seq, LitresPerPulse: s.k, Level: s.level, CountingSince: s.since,
		Count: s.count, LastIntervalMS: s.lastInterval, LastClosedMS: s.lastClosed, Lost: s.lost,
	}
	if s.lastPulse != nil {
		p := *s.lastPulse
		m.LastPulseAt = &p
	}
	return m
}

// close counts a litre at its closing and returns its pulse message.
func (s *sim) close(at time.Time) tanktemp.Meter {
	if s.lastPulse != nil {
		iv := at.Sub(*s.lastPulse).Milliseconds()
		s.lastInterval = &iv
	}
	s.count++
	s.level = tanktemp.LevelClosed
	p := at
	s.lastPulse = &p
	return s.emit(tanktemp.TriggerPulse, at)
}

// open ends the litre in progress at at and returns its open message.
func (s *sim) open(at time.Time) tanktemp.Meter {
	cl := at.Sub(*s.lastPulse).Milliseconds()
	s.lastClosed = &cl
	s.level = tanktemp.LevelOpen
	return s.emit(tanktemp.TriggerOpen, at)
}

func (s *sim) sweep(at time.Time) tanktemp.Meter { return s.emit(tanktemp.TriggerSweep, at) }

// litre is a whole litre: its pulse, then its open closed later.
func (s *sim) litre(at time.Time, closed time.Duration) []tanktemp.Meter {
	return []tanktemp.Meter{s.close(at), s.open(at.Add(closed))}
}

// run is a steady flow: n litres interval apart from start, each closed for
// duty × interval. It returns the messages and the time after the last.
func (s *sim) run(start time.Time, n int, interval time.Duration, duty float64) ([]tanktemp.Meter, time.Time) {
	var out []tanktemp.Meter
	at := start
	for i := 0; i < n; i++ {
		out = append(out, s.litre(at, time.Duration(float64(interval)*duty))...)
		at = at.Add(interval)
	}
	return out, at
}

func feedAll(c *Consumer, ms ...[]tanktemp.Meter) []Event {
	var ev []Event
	for _, batch := range ms {
		for _, m := range batch {
			ev = append(ev, c.Feed(m)...)
		}
	}
	return ev
}

func one(m tanktemp.Meter) []tanktemp.Meter { return []tanktemp.Meter{m} }

func ofType(ev []Event, ty EventType) []Event {
	var out []Event
	for _, e := range ev {
		if e.Type == ty {
			out = append(out, e)
		}
	}
	return out
}

func endedDraws(ev []Event) []Draw {
	var out []Draw
	for _, e := range ofType(ev, EventDrawEnded) {
		out = append(out, *e.Draw)
	}
	return out
}

// replay feeds a recording ('%I %t %p' lines, as tools/record.sh writes
// them, gzipped if the name ends .gz) to c: every topic ending /meter, in
// file order. It ends with a Tick a day after the last message, so every
// draw and continuous flow ends. Fixture-parity tests use it.
func replay(t *testing.T, c *Consumer, path string) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		z, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		defer z.Close()
		r = z
	}
	var ev []Event
	var last time.Time
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		parts := strings.SplitN(sc.Text(), " ", 3)
		if len(parts) < 3 || !strings.HasSuffix(parts[1], "/meter") {
			continue
		}
		m, err := tanktemp.DecodeMeter([]byte(parts[2]))
		if err != nil {
			t.Fatalf("%s:%d: %v", path, n, err)
		}
		ev = append(ev, c.Feed(m)...)
		if m.Timestamp.After(last) {
			last = m.Timestamp
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return append(ev, c.Tick(last.Add(24*time.Hour))...)
}
