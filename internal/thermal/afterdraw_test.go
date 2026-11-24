package thermal

import (
	"testing"
	"time"

	"github.com/sweeney/common/wire/boiler"
)

// afterDraw runs the left tank cooling for an hour, a draw ending at
// end, then the tank's temperature rising at riseKW from DrawSettle after
// the draw on, with the programmer's HW as hw; it returns when (after the
// draw's end) the tank was first judged heating, or -1.
func afterDraw(t *testing.T, riseKW float64, hw boiler.Switch) time.Duration {
	return afterDrawFrom(t, riseKW, hw, hw)
}

// afterDrawFrom is afterDraw with HW as before the draw ends, then as at it.
func afterDrawFrom(t *testing.T, riseKW float64, before, hw boiler.Switch) time.Duration {
	return afterDrawWith(t, riseKW, before, hw, boiler.Off)
}

// afterDrawWith is afterDrawFrom with CH as at the draw's end.
func afterDrawWith(t *testing.T, riseKW float64, before, hw, ch boiler.Switch) time.Duration {
	t.Helper()
	cfg := planConfig()
	tr := newTracker(t, cfg)
	s := newSynth(t)
	tr.Programmer(t0, before, boiler.Off)
	temp := 55.0
	at := t0
	for ; at.Before(t0.Add(time.Hour)); at = at.Add(30 * time.Second) {
		temp -= 0.002 // cooling, about −0.06 kW
		if _, err := tr.Sweep(s.sweep(left, at, temp)); err != nil {
			t.Fatal(err)
		}
	}
	tr.MeteredDraw(at, true)
	end := at.Add(2 * time.Minute)
	tr.MeteredDraw(end, false)
	tr.Programmer(end, hw, ch)
	perSweep := riseKW / kwhPerK / 3600 * 30 // °C per 30 s sweep
	temp -= 5                                // a big draw: its sweeps mustn't count
	for at = end.Add(10 * time.Second); at.Before(end.Add(cfg.PowerSettle + cfg.PowerMinSpan + 2*time.Minute)); at = at.Add(30 * time.Second) {
		if at.After(end.Add(cfg.DrawSettle)) {
			temp += perSweep
		}
		if _, err := tr.Sweep(s.sweep(left, at, temp)); err != nil {
			t.Fatal(err)
		}
		if st := mustStatus(t, tr, left); st.State == StateHeating {
			return at.Sub(end)
		}
	}
	return -1
}

// After a draw the power is unknown until PowerSettle + PowerMinSpan (17
// min), which hid reheats that began then (the owner, 24 Nov 2026: a
// reheat 12 min after a tap showed as heating only 9 min after the HW
// timer came on). With a heat source current, the readings since the draw
// say heating once they rise at HeatingOnAfterDrawKW or more: twice the
// most a draw's settling showed with no heat going in (0.72 kW, CH on in
// ch-only-no-heat).
func TestHeatingJudgedSoonAfterADraw(t *testing.T) {
	cfg := planConfig()
	hold := cfg.PowerSettle + cfg.PowerMinSpan
	if d := afterDraw(t, 4, boiler.On); d < 0 || d > cfg.DrawSettle+cfg.PowerMinSpan+time.Minute {
		t.Errorf("4 kW with HW on: heating at %v after the draw, want within %v (not the %v hold)", d, cfg.DrawSettle+cfg.PowerMinSpan+time.Minute, hold)
	}
	if d := afterDraw(t, 4, boiler.Off); d >= 0 && d < hold {
		t.Errorf("4 kW with no heat source: heating at %v, inside the %v hold", d, hold)
	}
	if d := afterDraw(t, 1, boiler.On); d >= 0 && d < hold {
		t.Errorf("1 kW with HW on (below the after-draw threshold): heating at %v, inside the %v hold", d, hold)
	}
}

// The coil keeps heating for a while after HW switches off (its afterglow
// reached 1.5 kW on the right; tanktemp's review of #15), so the fast rule
// needs a channel on now: within SourceHold of one isn't enough.
func TestAfterDrawNeedsAChannelOnNow(t *testing.T) {
	cfg := planConfig()
	if d := afterDrawFrom(t, 4, boiler.On, boiler.Off); d >= 0 && d < cfg.PowerSettle+cfg.PowerMinSpan {
		t.Errorf("4 kW just after HW went off: heating at %v, inside the hold", d)
	}
}

// CH heats neither tank since the right's valve was replaced, and with CH
// on and HW off the coil's afterglow reached 1.48 kW (tanktemp's review of
// #15): the fast rule needs HW on. A CH-only rise waits for the power.
func TestAfterDrawNeedsHW(t *testing.T) {
	cfg := planConfig()
	if d := afterDrawWith(t, 4, boiler.Off, boiler.Off, boiler.On); d >= 0 && d < cfg.PowerSettle+cfg.PowerMinSpan {
		t.Errorf("4 kW with only CH on: heating at %v, inside the hold", d)
	}
}
