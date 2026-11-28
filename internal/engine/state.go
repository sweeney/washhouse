package engine

import (
	"time"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/thermal"
)

// State is the system as of the latest input. Unknown is nil, never 0.
type State struct {
	// At is the engine's event time: the latest time any input carried.
	// Zero only before the first input.
	At    time.Time
	Tanks []TankState // in Config.Tanks order
	// Totals are unknown unless every tank's estimate is known. Each is
	// the tanks' summed: no fixed split limits what reaches the tap (a hot
	// tank supplies it while the other is cold; doc.go).
	StoredKWh    *float64
	UsableL40    *float64
	UsableL40Low *float64
	// LowHotWater: the system's UsableL40Low, with hysteresis: set below
	// Config.LowHotWaterL, cleared above it plus LowHotWaterMarginL, held
	// in between. Judged for the system, never one tank; unknown when the
	// total is (the held value is kept for when it is known again).
	LowHotWater *bool
	// Partial: while one or more tanks are blind (their estimate unknown)
	// and at least one isn't, the known ones' totals, a lower bound on the
	// system's; nil otherwise. The totals above stay nil while any is
	// blind. LowHotWater is false from it when its UsableL40Low clears
	// LowHotWaterL + LowHotWaterMarginL (the blind can only add to it),
	// else unknown; never true.
	Partial *Partial
	// SweepPending: the latest input is part of a tanktemp sweep that is
	// still arriving (its meter reading, or one tank's state while another
	// that isn't stale hasn't swept at that time yet). Not on the wire: the
	// publisher holds the state until the sweep is in, so one state goes
	// out per sweep (the owner, 24 Nov 2026).
	SweepPending bool
	Boiler       Boiler
	// AmbientC is the boiler room's latest reading, unknown once older
	// than Thermal.AmbientStale.
	AmbientC *float64
	// ActiveDraw is the running draw, or one the meter has ended whose
	// draw_ended hasn't been given yet; nil when neither.
	ActiveDraw *ActiveDraw
	Meter      MeterState
	Tanktemp   TanktempStatus
}

// TankState is one cylinder.
type TankState struct {
	DeviceID string
	// At is the tank's latest sweep (tanktemp's time); nil before the
	// first.
	At *time.Time
	// State is the thermal tracker's: heating, drawing, cooling or
	// unknown.
	State thermal.State
	// HeatSource is hw or ch while heating, else nil.
	HeatSource *thermal.Source
	// The estimate of the latest sweep; nil before one, once it is older
	// than Thermal.StaleAfter, or when it couldn't be estimated.
	StoredKWh    *float64
	MeanC        *float64
	TopC         *float64
	HotL         *float64
	UsableL40    *float64
	UsableL40Low *float64
	// PowerKW is the smoothed dE/dt; nil when unknown.
	PowerKW *float64
	// UAWPerK is the loss fit against the boiler room (the prior until
	// evidence comes).
	UAWPerK *float64
	// ProbesOK is how many probes the latest sweep's profile used, of
	// ProbesTotal; nil before the first sweep.
	ProbesOK    *int
	ProbesTotal *int
	// Fault is why the tank is blind (not estimated), nil while it is
	// estimated.
	Fault *Fault
	// LostSince: while the tank is lost (blind for TankFaultAfter, and not
	// yet estimated again for TankRestoreAfter), when its blind spell
	// began; nil otherwise. Not on the wire: /healthz reports it.
	LostSince *time.Time
}

// Partial is the known tanks' totals while some are blind.
type Partial struct {
	Missing                            []string // the blind tanks, in config order
	StoredKWh, UsableL40, UsableL40Low float64
}

// FaultReason is why a tank is blind.
type FaultReason string

// The fault reasons (docs/WIRE.md, Faults).
const (
	FaultProbes          FaultReason = "probes"           // its latest sweep had too few usable probes
	FaultStale           FaultReason = "stale"            // no sweep for Thermal.StaleAfter
	FaultTanktempOffline FaultReason = "tanktemp_offline" // stale, with tanktemp's status offline
)

// Fault is a blind tank's: since when (event time), why, and tanktemp's
// error codes in its latest sweep, counted. Lost: blind for TankFaultAfter.
type Fault struct {
	Since  time.Time
	Reason FaultReason
	Errors map[string]int
	Lost   bool
}

// Boiler is the programmer: true on, false off, nil unknown.
type Boiler struct {
	HW, CH *bool
}

// ActiveDraw is the running draw, with the same DrawID and StartedAt as
// its draw_started, so a consumer restarting mid-draw can recover it.
type ActiveDraw struct {
	DrawID      string
	StartedAt   time.Time // the first litre's closing
	LastLitreAt time.Time
	LitresSoFar float64
	FlowLPerMin *float64   // nil until the draw has two litres
	Kind        meter.Kind // provisional
}

// MeterState is the water meter as the engine last heard it.
type MeterState struct {
	At            *time.Time // the latest synced meter message's timestamp; nil before one
	Count         *uint64    // the current run's count
	CountingSince *time.Time // the current run
	// Litres is metered since the engine started, over every run.
	Litres *float64
	// ContinuousFlow is the running creep, or nil when there is none.
	ContinuousFlow *Flow
}

// TanktempStatus is tanktemp's latest status message.
type TanktempStatus struct {
	At      *time.Time // nil before one, and for the last will
	Online  *bool
	Version *string // nil before a status, and for the last will
}

// Flow is a continuous flow (creep): lone litres, never a draw.
type Flow struct {
	Since       time.Time // the first creep litre's closing
	Last        time.Time // the latest
	Litres      float64
	FlowLPerMin float64
}

// DrawEnded is a draw the meter has ended: the volume and the meter's
// account of it. Its attribution comes later, in DrawBalanced.
type DrawEnded struct {
	DrawID    string
	StartedAt time.Time // the first litre's closing
	EndedAt   time.Time // the last litre's closing
	DurationS float64
	// Litres are the meter's, whole litres: the volume. VolumeKind is
	// always "metered".
	Litres      float64
	VolumeKind  string
	FlowLPerMin *float64
	Kind        meter.Kind // final
	// The meter messages the draw covers.
	From, To meter.Ref
}

// DrawBalanced is an ended draw's attribution, with its draw_ended's
// DrawID and EndedAt.
type DrawBalanced struct {
	DrawID  string
	EndedAt time.Time // identical to its draw_ended's
	Tanks   []TankShare
	// OutletPeakC is the share-weighted outlet peak (the delivered
	// temperature); InletDropC the largest inlet drop; EnergyKWh the heat
	// the tanks lost by the balance sweep.
	OutletPeakC *float64
	InletDropC  *float64
	EnergyKWh   *float64
	// HeatBalanceL is the probes' cross-check of Litres, never the volume.
	HeatBalanceL        *float64
	HeatBalanceReliable bool
	Reasons             []draw.Reason
	MaskedByReheat      bool
}

// DrawRecord is one draw in Recent: its draw_ended, and its draw_balanced
// once there is one (nil until then).
type DrawRecord struct {
	DrawEnded
	Balanced *DrawBalanced
}

// TankShare is one tank's part in a draw. Share and Litres are unknown
// when the draw was masked (a reheat, or HW unknown) or a tank is missing.
type TankShare struct {
	DeviceID    string
	Share       *float64
	Litres      *float64
	OutletPeakC *float64
	InletDropC  *float64
}

// EventType says what an Event reports.
type EventType string

// The event types: washhouse's water/hot/events names (PLAN.md).
const (
	EventDrawStarted           EventType = "draw_started"
	EventDrawUpdated           EventType = "draw_updated"
	EventDrawEnded             EventType = "draw_ended"
	EventDrawBalanced          EventType = "draw_balanced"
	EventContinuousFlowStarted EventType = "continuous_flow_started"
	EventContinuousFlowEnded   EventType = "continuous_flow_ended"
	EventHeatingStarted        EventType = "heating_started"
	EventHeatingEnded          EventType = "heating_ended"
	EventTankStateChanged      EventType = "tank_state_changed"
	EventTankLost              EventType = "tank_lost"
	EventTankRestored          EventType = "tank_restored"
)

// Event is one thing the engine decided, at its event time. Exactly one
// of Draw, Ended, Balanced, Flow, Tank, Lost and Restored is set, by Type.
type Event struct {
	Type     EventType
	At       time.Time
	Draw     *ActiveDraw   // draw_started, draw_updated
	Ended    *DrawEnded    // draw_ended
	Balanced *DrawBalanced // draw_balanced
	Flow     *Flow         // continuous_flow_*
	Tank     *TankEvent    // heating_*, tank_state_changed
	Lost     *TankLost     // tank_lost
	Restored *TankRestored // tank_restored
}

// TankLost is a tank blind for TankFaultAfter: its fault then.
type TankLost struct {
	DeviceID string
	Reason   FaultReason
	Errors   map[string]int
	Since    time.Time
}

// TankRestored is a lost tank estimated again for TankRestoreAfter.
// Duration runs from LostSince to the first estimated sweep of the run
// that restored it: how long it was blind.
type TankRestored struct {
	DeviceID  string
	LostSince time.Time
	Duration  time.Duration
}

// TankEvent is a thermal change in one tank.
type TankEvent struct {
	DeviceID string
	State    thermal.State // after the change
	Prev     thermal.State
	// Source is the new heat source (for heating_ended, the episode's
	// last), or nil.
	Source  *thermal.Source
	PowerKW *float64
}
