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

// influxQueue is how many points wait for the client: a few hours of
// sweeps, then the newest are dropped rather than the loop held.
const influxQueue = 4096

// pointWriter is what washhouse uses of influxdb-client-go's async
// WriteAPI: it queues, and a background batch writes; a failed write comes
// out of Errors().
type pointWriter interface {
	WritePoint(p *write.Point)
	Flush()
	Errors() <-chan error
}

// influxSink writes what washhouse publishes to Influx (internal/influx's
// measurements). It never blocks the loop: the client's WritePoint can
// wait on a synchronous POST (statehouse's review of #8), so points go
// through a bounded queue to a goroutine of their own, and what the queue
// can't hold is dropped and counted. A write that fails is counted and
// shown on /healthz, never retried here; the client keeps a retry buffer
// (dropping its oldest when full) and reports at most one error at a time,
// so failed undercounts what was lost. Influx is history, not the
// service: a failure doesn't make washhouse degraded.
type influxSink struct {
	api    pointWriter
	close  func() // the client's; nil for a fake
	mapper influx.Mapper
	wall   func() time.Time

	queue   chan *write.Point
	drained chan struct{} // closed once the queue is closed and empty

	mu     sync.Mutex
	st     influxStatus
	closed bool // the queue is
}

// influxStatus is the sink on /healthz.
type influxStatus struct {
	Queued       uint64     `json:"queued"`
	Dropped      uint64     `json:"dropped"`  // points the queue couldn't hold
	Failed       uint64     `json:"failed"`   // writes Influx refused; see influxSink
	Unmapped     uint64     `json:"unmapped"` // messages with no points (a bug)
	LastQueuedAt *time.Time `json:"last_queued_at"`
	LastError    string     `json:"last_error,omitempty"`
	LastErrorAt  *time.Time `json:"last_error_at,omitempty"`
}

// influxOptions: a hung POST gives up sooner than the default 20 s.
func influxOptions() *influxdb2.Options {
	return influxdb2.DefaultOptions().SetHTTPRequestTimeout(10)
}

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

// newInfluxSink starts the queue's writer, and drains api's errors from
// the start.
func newInfluxSink(api pointWriter, closeFn func(), m influx.Mapper, wall func() time.Time) *influxSink {
	s := &influxSink{api: api, close: closeFn, mapper: m, wall: wall,
		queue: make(chan *write.Point, influxQueue), drained: make(chan struct{})}
	go func() {
		defer close(s.drained)
		for p := range s.queue {
			api.WritePoint(p)
		}
	}()
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
		at := s.wall().UTC()
		if err != nil {
			s.mu.Lock()
			s.st.Unmapped++
			s.st.LastError, s.st.LastErrorAt = err.Error(), &at
			s.mu.Unlock()
			continue
		}
		for _, p := range ps {
			wp := write.NewPoint(p.Measurement, p.Tags, p.Fields, p.Time)
			s.mu.Lock()
			queued := false
			if !s.closed {
				select {
				case s.queue <- wp:
					queued = true
				default:
				}
			}
			if queued {
				s.st.Queued++
				s.st.LastQueuedAt = &at
			} else {
				s.st.Dropped++
			}
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

// shutdown hands the client what is queued, flushes it, then closes the
// client; it gives up after wait (Influx down or hanging), leaving the
// client open as the process exits. Later writes are dropped.
func (s *influxSink) shutdown(wait time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { <-s.drained; s.api.Flush(); close(done) }()
	select {
	case <-done:
		if s.close != nil {
			s.close()
		}
	case <-time.After(wait):
	}
}
