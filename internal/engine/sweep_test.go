package engine

import (
	"testing"
	"time"

	"github.com/sweeney/common/wire/tanktemp"
)

// tanktemp sends a sweep as three messages with one timestamp: the meter's
// sweep reading, then each tank's state. The state is pending until every
// tank (that isn't stale) has swept at that time, so the publisher can send
// one state per sweep (the owner, 24 Nov 2026).
func TestSweepPending(t *testing.T) {
	e := newEngine(t, planConfig())
	m := &meterSim{since: t0.Add(-time.Hour)}
	lowFeed(t, e, t0, rightLow)
	if e.State().SweepPending {
		t.Error("both tanks swept at t0: pending")
	}
	at := t0.Add(30 * time.Second)
	e.Meter(m.msg(at, tanktemp.TriggerSweep, tanktemp.LevelOpen))
	if !e.State().SweepPending {
		t.Error("the meter's sweep reading, no tank yet: not pending")
	}
	if _, err := e.TankState(sweep(left, at, f(15), f(20), f(45), f(50), f(55))); err != nil {
		t.Fatal(err)
	}
	if !e.State().SweepPending {
		t.Error("the left tank swept, the right not yet: not pending")
	}
	r := rightLow
	if _, err := e.TankState(sweep(right, at, &r[0], &r[1], &r[2], &r[3], &r[4])); err != nil {
		t.Fatal(err)
	}
	if e.State().SweepPending {
		t.Error("both tanks swept: pending")
	}
	// A litre is no sweep: its state goes at once.
	for _, x := range m.litre(at.Add(5*time.Second), 2*time.Second) {
		e.Meter(x)
		if e.State().SweepPending {
			t.Errorf("a %s meter message: pending", x.Trigger)
		}
	}
}

// A tank gone silent (past StaleAfter) isn't waited for.
func TestSweepPendingSkipsAStaleTank(t *testing.T) {
	e := newEngine(t, planConfig())
	lowFeed(t, e, t0, rightLow)
	at := t0.Add(e.cfg.Thermal.StaleAfter + 30*time.Second)
	if _, err := e.TankState(sweep(left, at, f(15), f(20), f(45), f(50), f(55))); err != nil {
		t.Fatal(err)
	}
	if e.State().SweepPending {
		t.Error("the right tank is stale: still waited for")
	}
}
