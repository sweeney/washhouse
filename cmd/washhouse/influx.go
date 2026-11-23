package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"github.com/sweeney/washhouse/internal/influx"
	"github.com/sweeney/washhouse/internal/publish"
)

// influxWait bounds the flush at shutdown, inside systemd's stop timeout.
const influxWait = 5 * time.Second

// pointWriter is what washhouse uses of influxdb-client-go's async
// WriteAPI: it queues, and a background batch writes; a failed write comes
// out of Errors().
type pointWriter interface {
	WritePoint(p *write.Point)
	Flush()
	Errors() <-chan error
}

// influxSink writes what washhouse publishes to Influx (internal/influx's
// measurements). It never blocks the loop: points are queued, and a write
// that fails is counted and logged on /healthz, never retried here (the
// client keeps a retry buffer, and drops its oldest when that is full, so
// failed undercounts what was lost). Influx is history, not the service: a
// failure doesn't make washhouse degraded (statehouse's review).
type influxSink struct {
	api    pointWriter
	close  func() // the client's; nil for a fake
	mapper influx.Mapper
	wall   func() time.Time

	mu sync.Mutex
	st influxStatus
}

// influxStatus is the sink on /healthz.
type influxStatus struct {
	Queued       uint64     `json:"queued"`
	Failed       uint64     `json:"failed"` // writes, not points; see influxSink
	LastQueuedAt *time.Time `json:"last_queued_at"`
	LastError    string     `json:"last_error,omitempty"`
	LastErrorAt  *time.Time `json:"last_error_at,omitempty"`
}

func influxOptions() *influxdb2.Options { return influxdb2.DefaultOptions() }

// dialInflux is the sink for the influx section: the token is read from
// its file, and the client made (it connects on its first write).
func dialInflux(c influxConfig, site, base string, wall func() time.Time) (*influxSink, error) {
	b, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("influx.token_file: %w", err)
	}
	tok := string(bytes.TrimSpace(b))
	if tok == "" {
		return nil, errors.New("influx.token_file is empty")
	}
	client := influxdb2.NewClientWithOptions(c.URL, tok, influxOptions())
	return newInfluxSink(client.WriteAPI(c.Org, c.Bucket), client.Close, influx.Mapper{Base: base, Site: site}, wall), nil
}

// newInfluxSink drains api's errors from the start: unread, they would
// block the client's writes behind them (statehouse's review).
func newInfluxSink(api pointWriter, closeFn func(), m influx.Mapper, wall func() time.Time) *influxSink {
	s := &influxSink{api: api, close: closeFn, mapper: m, wall: wall}
	errs := api.Errors()
	go func() {
		for err := range errs {
			s.failed(err)
		}
	}()
	return s
}

func (s *influxSink) failed(err error) {
	at := s.wall().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Failed++
	s.st.LastError, s.st.LastErrorAt = err.Error(), &at
}

// write queues the points for what was just published. A nil sink is
// Influx off.
func (s *influxSink) write(msgs []publish.Message) {
	if s == nil {
		return
	}
	for _, m := range msgs {
		ps, err := s.mapper.Points(m)
		if err != nil {
			s.failed(err)
			continue
		}
		for _, p := range ps {
			s.api.WritePoint(write.NewPoint(p.Measurement, p.Tags, p.Fields, p.Time))
			at := s.wall().UTC()
			s.mu.Lock()
			s.st.Queued++
			s.st.LastQueuedAt = &at
			s.mu.Unlock()
		}
	}
}

// report is the sink on /healthz: null when Influx is off.
func (s *influxSink) report() *influxStatus {
	if s == nil {
		return nil
	}
	st := s.status()
	return &st
}

func (s *influxSink) status() influxStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// shutdown flushes what is queued, then closes the client; it gives up
// after wait (Influx down, the flush retrying), leaving the client open
// as the process exits.
func (s *influxSink) shutdown(wait time.Duration) {
	if s == nil {
		return
	}
	done := make(chan struct{})
	go func() { s.api.Flush(); close(done) }()
	select {
	case <-done:
		if s.close != nil {
			s.close()
		}
	case <-time.After(wait):
	}
}
