// Package engine wires washhouse's pure core together: decoded inputs in,
// the system's state and its events out (PLAN.md, internal/engine).
//
// It is pure: no I/O, no goroutines, no wall clock. Time is event time,
// taken from the inputs (tanktemp's sweep and meter timestamps, the
// programmer's switch times, the climate sensor's receive time) and from
// Tick, which the caller drives. The engine's clock only moves forward: it
// is the latest event time any input has carried. One goroutine feeds it,
// in arrival order; it is not safe for concurrent use.
//
// # Inputs
//
//   - TankState: a tanktemp sweep. It goes through the tank's model
//     (package tank) to the thermal tracker and the draw attributor. Before
//     it, the meter consumer is ticked to the sweep's time, so a draw that
//     ended before the sweep is known to the tracker when it judges it. A
//     sweep with an unsynced clock, or no later than that tank's last, is
//     skipped: its time can't be trusted, or it is not news.
//   - Meter: a tanktemp meter message, to the meter consumer (package
//     meter). Its draws become draw_started and draw_updated here, and the
//     tracker's MeteredDraw; its continuous flows pass through.
//   - Status: tanktemp's status, recorded (online, version).
//   - BoilerEvent and BoilerSystem: the programmer's HW and CH. A switch
//     event always applies. A system message applies only if it is a
//     HEARTBEAT with ready true: after a reconnect the retained system
//     message can be RECONNECTED (or a last will), which carries no switch
//     states, and a heartbeat before the GPIO baseline (ready false) can't
//     be trusted. Neither changes what is known. Unknown stays unknown
//     (nil in the state) until a heartbeat or event says otherwise.
//   - Ambient: a boiler-room temperature, for the tracker's loss fit.
//   - Tick: event time moves on with no input (a draw, a continuous flow
//     or a pending attribution can end; a tank can go stale).
//
// At the end of a replay, Pending says whether a draw is still running or
// waiting for its attribution, and a Tick Settle after the last input ends
// and attributes it.
//
// Each input returns the events it decided, sorted by event time. Across
// calls an event can be dated before one already returned: a fact decided
// late carries its own time (draw_started is dated at the draw's first
// litre, which the meter can confirm up to a minute later).
//
// # Events
//
//   - draw_started (at the first metered litre's closing), draw_updated
//     (at a litre, at most every meter.Config.UpdateEvery): the running
//     draw, with a provisional kind.
//   - draw_ended: the draw with its attribution (package draw): each
//     tank's share, the outlet peak, the inlet drop, the energy and the
//     heat-balance cross-check with its reliability and reasons, and the
//     meter seq range. It is emitted when the attribution is complete, and
//     the draw package decides when that is: once every tank has its
//     balance sweep (the last litre + Draw.BalanceSettle), or the next draw
//     cuts it short, or the wait runs out (BalanceSettle + MaxSweepGap
//     after the last litre). So draw_ended comes some minutes after the
//     last litre (about BalanceSettle, 8 min, with the plan's values), and
//     after the draw has left active_draw (the meter ends a draw DrawEnd
//     after its last litre). Its ended_at is the last litre's closing; the
//     event's own time is when it was decided.
//   - continuous_flow_started and continuous_flow_ended: creep (package
//     meter). Never a draw.
//   - heating_started, heating_ended and tank_state_changed: the thermal
//     tracker's (package thermal).
//
// # State
//
// State reports the system as of the latest input: per tank its estimate,
// thermal state, power and loss fit; the totals; the programmer; the room;
// the running draw; the meter. Unknown is nil, never 0. A tank whose last
// sweep is older than Thermal.StaleAfter, or whose last sweep couldn't be
// estimated, has an unknown estimate; the totals are unknown unless every
// tank's estimate is known. low_hot_water is judged for the system, never
// one tank: the total usable_l40_low below LowHotWaterL. The room reading
// is unknown once older than Thermal.AmbientStale.
//
// TODO(phase 2 follow-up): top_c is the estimate's profile top. PLAN.md
// wants the outlet probe's last draw peak, decayed; the seam is the outlet
// probe's reading before the profile is built (package profile).
package engine
