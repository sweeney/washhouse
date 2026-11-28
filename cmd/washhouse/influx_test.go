package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"github.com/sweeney/washhouse/internal/influx"
	"github.com/sweeney/washhouse/internal/publish"
)

// fakeInflux is the pointWriter the tests use: it keeps each point's line
// protocol, and while failing reports an error for each write instead.
type fakeInflux struct {
	mu      sync.Mutex
	lines   []string
	failing bool
	errs    chan error
	flushed int
	hang    chan struct{} // when set, Flush waits on it
	stuck   chan struct{} // when set, WritePoint waits on it (Influx hanging)
	atFlush []int         // how many points had been written at each Flush
}

func newFakeInflux() *fakeInflux { return &fakeInflux{errs: make(chan error, 64)} }

func (f *fakeInflux) WritePoint(p *write.Point) {
	f.mu.Lock()
	stuck := f.stuck
	f.mu.Unlock()
	if stuck != nil {
		<-stuck
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		f.errs <- errors.New("fake: write failed")
		return
	}
	f.lines = append(f.lines, strings.TrimSuffix(write.PointToLineProtocol(p, time.Nanosecond), "\n"))
}

func (f *fakeInflux) Flush() {
	f.mu.Lock()
	f.flushed++
	f.atFlush = append(f.atFlush, len(f.lines))
	hang := f.hang
	f.mu.Unlock()
	if hang != nil {
		<-hang
	}
}

func (f *fakeInflux) Errors() <-chan error { return f.errs }

func (f *fakeInflux) delivered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

func (f *fakeInflux) fail(b bool) { f.mu.Lock(); f.failing = b; f.mu.Unlock() }

// fakeInfluxServer is Influx's write endpoint: it keeps the lines written,
// and while failing answers 503.
type fakeInfluxServer struct {
	*httptest.Server
	mu      sync.Mutex
	lines   []string
	failing atomic.Bool
}

func newFakeInfluxServer(t *testing.T) *fakeInfluxServer {
	s := &fakeInfluxServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/write" || r.Header.Get("Authorization") != "Token tok" {
			http.Error(w, `{"code":"unauthorized","message":"no"}`, http.StatusUnauthorized)
			return
		}
		if s.failing.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"code":"unavailable","message":"down"}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				s.lines = append(s.lines, l)
			}
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *fakeInfluxServer) delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// testPointWriter is the contract every pointWriter passes, the real
// client and the fake alike: a written point is delivered by a Flush, as
// its line protocol; a write that fails comes out of Errors().
func testPointWriter(t *testing.T, api pointWriter, delivered func() []string, fail func(bool)) {
	t.Helper()
	errs := api.Errors()
	at := time.Date(2001, 1, 1, 0, 0, 1, 500e6, time.UTC)
	p1 := write.NewPoint("hot_water_tank", map[string]string{"site": "home", "device_id": "a"},
		map[string]any{"state": "cooling", "stored_kwh": 8.0, "probes_ok": int64(5)}, at)
	p2 := write.NewPoint("hot_water_system", map[string]string{"site": "home"}, map[string]any{"drawing": int64(1)}, at)
	api.WritePoint(p1)
	api.WritePoint(p2)
	api.Flush()
	want := []string{
		`hot_water_tank,device_id=a,site=home probes_ok=5i,state="cooling",stored_kwh=8 978307201500000000`,
		`hot_water_system,site=home drawing=1i 978307201500000000`,
	}
	waitFor(t, "the points delivered", func() bool { return strings.Join(delivered(), "\n") == strings.Join(want, "\n") })

	fail(true)
	api.WritePoint(p1)
	api.Flush()
	select {
	case err := <-errs:
		if err == nil {
			t.Error("a nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a failed write reported no error")
	}
}

func TestPointWriterContract(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		f := newFakeInflux()
		testPointWriter(t, f, f.delivered, f.fail)
	})
	t.Run("influxdb-client-go", func(t *testing.T) {
		s := newFakeInfluxServer(t)
		c := influxdb2.NewClientWithOptions(s.URL, "tok", influxOptions().SetMaxRetries(0))
		defer c.Close()
		testPointWriter(t, c.WriteAPI("org", "bucket"), s.delivered, s.failing.Store)
	})
}

// stateAt is a state published i seconds into the fake epoch, and a
// status (which has no points). A state's row is held until a later one
// arrives (influx.Rows), so the tests publish at successive times.
func stateAt(i int) []publish.Message {
	ts := time.Date(2001, 1, 1, 0, 0, i, 0, time.UTC).Format("2006-01-02T15:04:05.000Z")
	return []publish.Message{
		{Topic: "water/hot/state", Payload: []byte(`{"schema":"net.swee.washhouse.state.v1","timestamp":"` + ts + `","stored_kwh":null,"usable_l40":null,"usable_l40_low":null,"low_hot_water":null,"boiler":{"hw":true,"ch":null},"ambient_c":null,"active_draw":null,"meter":{"timestamp":null,"counting_since":null,"count":null,"litres_since_start":null,"continuous_flow":null},"tanktemp":{"timestamp":null,"online":null,"version":null},"tanks":[]}`), Retained: true},
		{Topic: "water/hot/status", Payload: []byte(`{}`), Retained: true},
	}
}

var messages = stateAt(0)

// The sink maps what was published and queues it, and counts what it
// queued and what failed, for /healthz.
func TestInfluxSink(t *testing.T) {
	f := newFakeInflux()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := newInfluxSink(f, nil, influx.Mapper{Base: "water/hot", Site: "home"}, func() time.Time { return now })
	if st := s.status(); st.Queued != 0 || st.LastQueuedAt != nil || st.Failed != 0 {
		t.Errorf("before: %+v", st)
	}
	s.write(stateAt(0))
	if st := s.status(); st.Queued != 0 {
		t.Errorf("the row is held for its final content: %+v", st)
	}
	s.write(stateAt(1))
	waitFor(t, "the point", func() bool { return len(f.delivered()) == 1 })
	if got := f.delivered(); got[0] != `hot_water_system,site=home boiler_hw=1i,continuous_flow=0i,drawing=0i,tanks_missing=0i 978307200000000000` {
		t.Errorf("delivered %q", got)
	}
	if st := s.status(); st.Queued != 1 || st.LastQueuedAt == nil || !st.LastQueuedAt.Equal(now) || st.Failed != 0 {
		t.Errorf("after one: %+v", st)
	}

	f.fail(true)
	s.write(stateAt(2))
	waitFor(t, "the failure counted", func() bool { return s.status().Failed == 1 })
	if st := s.status(); st.LastError != "fake: write failed" || st.LastErrorAt == nil || !st.LastErrorAt.Equal(now) {
		t.Errorf("after a failure: %+v", st)
	}

	// A message that doesn't map is counted on its own, not a crash.
	s.write([]publish.Message{{Topic: "water/hot/state", Payload: []byte(`{`)}})
	if st := s.status(); st.Failed != 1 || st.Unmapped != 1 || !strings.Contains(st.LastError, "water/hot/state") {
		t.Errorf("after a bad message: %+v", st)
	}

	// A nil sink is Influx off.
	var off *influxSink
	off.write(messages)
	off.shutdown(time.Second)
	if off.report() != nil {
		t.Errorf("Influx off on /healthz: %+v, want null", off.report())
	}
	if st := s.report(); st == nil || st.Queued != 2 || st.Failed != 1 {
		t.Errorf("on /healthz: %+v", st)
	}
}

// A hanging Influx never stalls the loop (statehouse's review of #8: the
// client's WritePoint blocks behind a synchronous POST): write returns at
// once, and what the queue can't hold is dropped and counted.
func TestInfluxSinkNeverBlocks(t *testing.T) {
	f := newFakeInflux()
	f.stuck = make(chan struct{})
	s := newInfluxSink(f, nil, influx.Mapper{Base: "water/hot"}, time.Now)
	done := make(chan struct{})
	go func() {
		for i := range influxQueue + 11 { // the last is held
			s.write(stateAt(i)[:1])
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("write blocked behind a hanging Influx")
	}
	if st := s.status(); st.Dropped < 9 || st.Queued+st.Dropped != influxQueue+10 {
		t.Errorf("queued %d, dropped %d", st.Queued, st.Dropped)
	}
	// Shutdown gives up on it too, and a write after shutdown is dropped.
	start := time.Now()
	s.shutdown(50 * time.Millisecond)
	if d := time.Since(start); d > time.Second {
		t.Errorf("shutdown took %v", d)
	}
	s.write(messages[:1])
	close(f.stuck)
}

// shutdown flushes, then closes the client; a Flush that hangs (Influx
// down, the retry buffer full) is given up after the wait, so the stop
// stays inside systemd's timeout.
func TestInfluxSinkShutdown(t *testing.T) {
	f := newFakeInflux()
	closed := 0
	s := newInfluxSink(f, func() { closed++ }, influx.Mapper{Base: "water/hot"}, time.Now)
	s.shutdown(time.Second)
	if f.flushed != 1 || closed != 1 {
		t.Errorf("flushed %d, closed %d", f.flushed, closed)
	}

	// What is still queued is handed to the client before the flush.
	f = newFakeInflux()
	f.stuck = make(chan struct{})
	s = newInfluxSink(f, nil, influx.Mapper{Base: "water/hot"}, time.Now)
	for i := range 3 { // two queued, the last held: all three before the flush
		s.write(stateAt(i)[:1])
	}
	time.AfterFunc(50*time.Millisecond, func() { close(f.stuck) })
	s.shutdown(5 * time.Second)
	if len(f.atFlush) != 1 || f.atFlush[0] != 3 {
		t.Errorf("points written at the flush: %v, want [3]", f.atFlush)
	}

	f = newFakeInflux()
	f.hang = make(chan struct{})
	defer close(f.hang)
	s = newInfluxSink(f, func() { t.Error("closed under a hung flush") }, influx.Mapper{Base: "water/hot"}, time.Now)
	start := time.Now()
	s.shutdown(50 * time.Millisecond)
	if d := time.Since(start); d > time.Second {
		t.Errorf("shutdown took %v", d)
	}
}

// Writes from the loop while the drain goroutine counts failures and a
// reader asks for the status: run under -race.
func TestInfluxSinkConcurrent(t *testing.T) {
	f := newFakeInflux()
	f.fail(true)
	s := newInfluxSink(f, nil, influx.Mapper{Base: "water/hot"}, time.Now)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 21 { // the last is held
			s.write(stateAt(i))
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			_ = s.status()
		}
	}()
	wg.Wait()
	waitFor(t, "every failure counted", func() bool { return s.status().Failed == 20 })
}

func TestDialInflux(t *testing.T) {
	srv := newFakeInfluxServer(t)
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := dialInflux(influxConfig{URL: srv.URL, Org: "o", Bucket: "b", TokenFile: tok}, "home", "water/hot", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	s.write(messages)
	s.shutdown(5 * time.Second)
	if got := srv.delivered(); len(got) != 1 || !strings.HasPrefix(got[0], "hot_water_system,site=home ") {
		t.Errorf("delivered %q", got)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []influxConfig{
		{URL: srv.URL, Org: "o", Bucket: "b", TokenFile: filepath.Join(dir, "missing")},
		{URL: srv.URL, Org: "o", Bucket: "b", TokenFile: empty},
	} {
		if _, err := dialInflux(c, "home", "water/hot", time.Now); err == nil {
			t.Errorf("%+v: no error", c)
		}
	}
}
