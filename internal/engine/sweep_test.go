package engine

import (
	"testing"
	"time"

	"github.com/sweeney/common/wire/boiler"
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

// A lone meter sweep reading with no tank states after it (a sweep
// cancelled at shutdown, tanktemp's last reading at SHUTDOWN, a lost tank
// message) mustn't hold State indefinitely: the hold ends StaleAfter past
// the sweep's time, and tanktemp's status or the programmer end it at once
// (tanktemp's review of #14).
func TestSweepPendingIsBounded(t *testing.T) {
	setup := func() *Engine {
		e := newEngine(t, planConfig())
		lowFeed(t, e, t0, rightLow)
		m := &meterSim{since: t0.Add(-time.Hour)}
		e.Meter(m.msg(t0.Add(30*time.Second), tanktemp.TriggerSweep, tanktemp.LevelOpen))
		if !e.State().SweepPending {
			t.Fatal("a lone meter sweep reading: not pending")
		}
		return e
	}
	e := setup()
	// The clock moves event time only after a silence; a minute past the
	// bound is past both.
	e.Tick(t0.Add(30*time.Second + e.cfg.Thermal.StaleAfter + time.Minute))
	if e.State().SweepPending {
		t.Error("still pending after the clock moved on")
	}
	e = setup()
	e.Status(tanktemp.Status{Online: false, Event: tanktemp.EventShutdown})
	if e.State().SweepPending {
		t.Error("still pending after tanktemp's status")
	}
	e = setup()
	e.BoilerEvent(boiler.Event{Timestamp: t0.Add(31 * time.Second), HW: boiler.On, CH: boiler.Off})
	if e.State().SweepPending {
		t.Error("still pending after a programmer event")
	}
	// A room reading doesn't end it; the bound does.
	e = setup()
	e.Ambient(t0.Add(31*time.Second), 18)
	if !e.State().SweepPending {
		t.Error("a room reading ended the hold")
	}
	e.Tick(t0.Add(31*time.Second + e.cfg.Thermal.StaleAfter + time.Minute))
	if e.State().SweepPending {
		t.Error("still pending past StaleAfter")
	}
}

// A tank state the model refuses (too few probes) still counts as that
// tank's part of the sweep.
func TestARefusedSweepCountsForTheSweep(t *testing.T) {
	e := newEngine(t, planConfig())
	lowFeed(t, e, t0, rightLow)
	at := t0.Add(30 * time.Second)
	if _, err := e.TankState(sweep(left, at, f(15), f(20), f(45), f(50), f(55))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.TankState(sweep(right, at, f(40), nil, nil, nil, nil)); err == nil {
		t.Fatal("a one-probe sweep: no error")
	}
	if e.State().SweepPending {
		t.Error("the right tank's refused state didn't count: still pending")
	}
}
