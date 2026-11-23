// Package meter is washhouse's consumer of tanktemp's water meter
// (`water/hot/tanktemp/meter`, WIRE.md ## Meter): it turns meter messages
// into metered litres, draws, creep and meter health.
//
// It is pure: no I/O, no goroutines, no wall clock. Time is event time,
// taken only from the messages (and from Tick, which the caller drives).
// Feed it decoded messages in arrival order; each call returns the events it
// decided. A replay of a recording is a loop over Feed and a final Tick.
//
// The rules, and where they come from:
//   - runs and de-duplication (WIRE.md): a message is keyed on
//     (counting_since, seq). A new counting_since is a new run (a restart),
//     and litres add across runs. A seq at or below the run's highest seen is
//     a retained replay or duplicate, and not news. On a cold start (no run
//     seen yet), the first message of a run joined mid-run (seq > 1) is the
//     baseline: litres counted before it are not news. A new run first seen
//     at seq > 1 after a run this consumer saw is a restart it lived through
//     with messages lost: count starts at 0 per run, so everything the run
//     counted is news, a count jump like any other.
//   - litres (WIRE.md): only count measures litres (count × litres_per_pulse),
//     and only as count rises within a run; a change in last_pulse_at alone
//     is never a litre (tanktemp's open can round last_pulse_at 1 ms off its
//     pulse's). A jump of more than one is skipped messages, not lost
//     litres: the skipped pulses are counted, untimed, with the timed litre
//     that revealed them.
//   - clock_synced false (WIRE.md): a Pi with no RTC runs, until NTP
//     syncs, on the clock saved at shutdown, which can be earlier than the
//     last run's times. A message stamped so is de-duplicated and its count
//     taken (Health.Litres, Health.Unsynced), but its times are used for
//     nothing: no litre event, no draw, no event time. Litres are timed
//     again from the first synced message. (Simpler than tracking the
//     last litre per run, and it also covers a clock that is ahead.)
//   - draws: a draw ends DrawEnd (event time) after its last litre, so a
//     litre less than DrawEnd after the previous one joins it. One rule, not
//     two: PLAN.md's 36 s join gap split a real bath at a 40.7 s pause, and
//     tools/meter_audit.py uses the same single rule. Flow is from the median
//     interval between litres, as meter_audit.py computes it. Kind: bath at
//     BathFlowLPerMin and BathMinLitres or more, else shower at
//     ShowerMinFlowLPerMin, ShowerMinLitres and ShowerMinDuration or more,
//     else tap. A steady flow with litres under DrawEnd apart is a draw
//     however slow (1.2 L/min is one), so the shower's minimum flow is what
//     keeps a dribbling tap from reading as a long shower.
//   - creep (PLAN.md): a litre that would start a draw, but whose reed stayed
//     closed CreepClosed or longer, and that no litre follows within DrawEnd,
//     is creep, never a draw. (The last clause is a judgement from live data:
//     a slow-opening tap held the first litre of a 5 L draw closed for
//     13.7 s, and the next litre came 23 s after it.) Two creep litres within
//     CreepWindow start a continuous flow, which ends CreepWindow after the
//     last creep litre.
//
// Every threshold is in Config; none is in code.
package meter

import (
	"strconv"
	"time"

	"github.com/sweeney/common/wire/tanktemp"
)

// Config holds every threshold the consumer uses. It has no defaults: the
// values come from washhouse's config, and Validate refuses a zero.
type Config struct {
	// DeviceID: the meter this consumer reads (its device_id, tanktemp's
	// key in devices_<site>). tanktemp's meters are a list, and runs are
	// keyed on counting_since alone, so a message from any other device_id
	// is refused (Health.OtherDevices), never merged into this one's counts.
	DeviceID string
	// DrawEnd: a draw ends this long after its last litre (60 s); a litre
	// less than this after the last joins the draw.
	DrawEnd time.Duration
	// BathFlowLPerMin and BathMinLitres: a draw at this median flow or
	// more, and of at least these litres, is a bath (10 L/min, 20 L; real
	// baths here are 72–87 L). The litres keep a short full-flow hot tap (a
	// few litres at 10 L/min or more) a tap.
	BathFlowLPerMin float64
	BathMinLitres   float64
	// ShowerMinLitres, ShowerMinDuration and ShowerMinFlowLPerMin: a draw
	// that isn't a bath, with at least these litres, this duration and this
	// median flow, is a shower (20 L, 180 s, 3 L/min). Real showers here run
	// at 4–6.4 L/min and taps at 3.5–5; the flow keeps a tap left dribbling
	// (or circulation through the meter) from reading as a long shower.
	ShowerMinLitres      float64
	ShowerMinDuration    time.Duration
	ShowerMinFlowLPerMin float64
	// CreepClosed: a litre whose reed stayed closed this long or longer,
	// while no draw is running, and that no litre follows within DrawEnd,
	// is creep (10 s).
	CreepClosed time.Duration
	// CreepWindow: two creep litres within this start a continuous flow, and
	// it ends this long after the last creep litre (2700 s).
	CreepWindow time.Duration
	// StaleAfter: the meter is stale once its last message is older than
	// this (90 s: 3 × tanktemp's 30 s sweep).
	StaleAfter time.Duration
	// UpdateEvery: while a draw runs, a draw_updated event at most this
	// often, on a litre. 0 turns draw_updated off.
	UpdateEvery time.Duration
}

// Ref names one meter message: the de-duplication key.
type Ref struct {
	CountingSince time.Time
	Seq           uint64
}

// DrawID names the draw whose first litre was seen in message r:
// counting_since (UTC, to the second) and seq, e.g. "20010101T070000Z-2".
// It is a fact of the meter, not of this process, so a washhouse restart
// can't reuse an earlier draw's ID, and the same messages give the same ID.
func DrawID(r Ref) string {
	return r.CountingSince.UTC().Format("20060102T150405Z") + "-" + strconv.FormatUint(r.Seq, 10)
}

// Kind is a draw's kind.
type Kind string

// Draw kinds.
const (
	KindTap    Kind = "tap"
	KindShower Kind = "shower"
	KindBath   Kind = "bath"
)

// EventType says what an Event reports.
type EventType string

// Event types. The draw and continuous-flow names are washhouse's
// `water/hot/events` names (PLAN.md).
const (
	EventLitre                 EventType = "litre"
	EventDrawStarted           EventType = "draw_started"
	EventDrawUpdated           EventType = "draw_updated"
	EventDrawEnded             EventType = "draw_ended"
	EventContinuousFlowStarted EventType = "continuous_flow_started"
	EventContinuousFlowEnded   EventType = "continuous_flow_ended"
)

// Event is one thing the consumer decided. At is the event time of the
// fact (a litre's closing, a draw's first litre, the moment a draw's
// DrawEnd ran out), which can be earlier than the message that decided it.
// Exactly one of Litre, Draw and Flow is set, by Type.
type Event struct {
	Type  EventType
	At    time.Time
	Litre *Litre
	Draw  *Draw
	Flow  *ContinuousFlow
}

// Litre is one metered litre (one pulse), or a timed pulse together with
// the untimed ones a count jump revealed with it.
type Litre struct {
	At      time.Time // the counted closing (last_pulse_at); the message time when Skipped == Pulses (none timed)
	Pulses  int       // 1 + Skipped
	Skipped int       // pulses whose own messages weren't seen
	Litres  float64   // Pulses × litres_per_pulse
	Total   float64   // litres metered since the consumer started, all runs
	// ClosedMS is how long the reed stayed closed in this litre, if known
	// when it was classified.
	ClosedMS *int64
	Creep    bool
	DrawID   string // "" for a creep litre
	Ref      Ref    // the message that revealed it
}

// Draw is a draw, as of the event that carries it.
type Draw struct {
	ID          string    // DrawID(From): stable across washhouse restarts
	Start, End  time.Time // the first and last litres' closings
	Duration    time.Duration
	Pulses      int
	Litres      float64  // whole metered litres
	FlowLPerMin *float64 // median flow; nil until the draw has two litres
	Kind        Kind     // provisional until Ended
	From, To    Ref      // the meter messages it covers
	Ended       bool
}

// ContinuousFlow is a run of creep litres.
type ContinuousFlow struct {
	Since       time.Time // the first creep litre's closing
	Last        time.Time // the latest creep litre's closing
	Pulses      int
	Litres      float64
	FlowLPerMin float64 // from the median interval between creep litres
	Ended       bool
}

// Health is the meter's health as the consumer sees it.
type Health struct {
	Messages int // messages fed, duplicates included
	// OtherDevices: messages refused for a device_id other than
	// Config.DeviceID. Nothing else in Health counts them.
	OtherDevices int
	Duplicates   int    // de-duplicated on (counting_since, seq)
	StaleRuns    int    // messages from an earlier run, after a newer one began
	Runs         int    // counting runs seen
	MissingSeq   uint64 // seq gaps within runs: messages never seen
	CountJumps   int    // messages whose count rose by more than one
	Skipped      int    // pulses seen only as a count jump
	CountFell    int    // messages whose count fell within a run (a contract breach)
	// Unattributed is pulses counted with no litre time and no draw to
	// join.
	Unattributed int
	// Unsynced is pulses counted from messages stamped clock_synced false:
	// in Litres, but in no litre event and no draw.
	Unsynced int
	Litres   float64 // metered since the consumer started, all runs

	// The latest message.
	Run           Ref
	LastTimestamp time.Time
	ClockSynced   bool
	Level         tanktemp.Level
	// tanktemp's counters for the current run.
	Noise, Short, Glitches, Lost uint64
	// LostBoundLitres: with lost > 0 the count is a lower bound, short by at
	// most 2 × lost pulses (WIRE.md).
	LostBoundLitres float64
}
