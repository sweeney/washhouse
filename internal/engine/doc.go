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
//     event applies, and so does a system message that is a HEARTBEAT with
//     ready true (boiler.System.ReadyHeartbeat, the one rule the draw
//     package's fixture replay uses too): a RECONNECTED carries no switch
//     states, and a heartbeat before the GPIO baseline (ready false) can't
//     be trusted. A SHUTDOWN (the last will, or a clean shutdown) makes
//     both unknown. None of these applies if it is older than the last
//     programmer input applied, so a retained message delivered after a
//     newer heartbeat can't roll the state back, and a retained last will
//     (dated when boiler-sensor last connected, often weeks back) is
//     ignored. More than BoilerMissedHeartbeats heartbeat intervals of
//     event time without an input applied makes both unknown, dated at that
//     limit; the interval is the heartbeat's own config.heartbeat_ms when
//     it carries one, else BoilerHeartbeat. Unknown stays unknown (nil in
//     the state) until a heartbeat or event says otherwise.
//   - Ambient: a boiler-room temperature, for the tracker's loss fit.
//   - Tick: event time moves on with no input (a draw, a continuous flow
//     or a pending attribution can end; a tank can go stale).
//
// A last will is dated when boiler-sensor connected, so one that fires
// while washhouse is listening is dated before the latest heartbeat and is
// ignored like a retained one (quiet-night has one, 4 s before
// boiler-sensor reconnects). Until the service can tell a live will from a
// retained one (MQTT's retain flag), only the silence rule catches
// boiler-sensor dropping out.
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
//   - draw_ended: the meter's draw (litres, kind, flow, started_at,
//     ended_at, duration and the meter seq range), with none of the
//     attribution. It is emitted once the meter has ended the draw (DrawEnd
//     after its last litre) and every tank's share sweep is in (the last
//     litre + Draw.ShareSettle), or the wait for those runs out (that +
//     MaxSweepGap), or the next draw starts: about a minute after the last
//     litre with the plan's values. Until then the draw stays in
//     active_draw (the meter forgets it at its end), and it leaves
//     active_draw in the same input that emits draw_ended.
//   - draw_balanced: the draw's attribution (package draw), with the same
//     draw_id and an identical ended_at: each tank's share and litres
//     (unknown on a masked draw), the outlet peak, the inlet drop, the
//     energy and the heat-balance cross-check with its reliability and
//     reasons, and masked_by_reheat. It is emitted when the attribution is
//     complete: once every tank has its balance sweep (the last litre +
//     Draw.BalanceSettle, 8 min), or the next draw cuts it short, or the
//     wait runs out (BalanceSettle + MaxSweepGap after the last litre).
//   - continuous_flow_started and continuous_flow_ended: creep (package
//     meter). Never a draw.
//   - heating_started, heating_ended and tank_state_changed: the thermal
//     tracker's (package thermal).
//
// Every event's own time is when it was decided; draw_ended's and
// draw_balanced's ended_at is the last litre's closing (statehouse's
// ClearSignal takes it).
//
// # State
//
// State reports the system as of the latest input: per tank its estimate,
// thermal state, power and loss fit; the totals; the programmer; the room;
// the running draw (or the ended one awaiting draw_ended); the meter.
// Unknown is nil, never 0. A tank whose last sweep is older than
// Thermal.StaleAfter, or whose last sweep couldn't be estimated, has an
// unknown estimate; the totals are unknown unless every tank's estimate is
// known. The room reading is unknown once older than Thermal.AmbientStale.
//
// The totals: stored_kwh is the tanks summed. The usable litres are not:
// the tanks are in parallel with one merged outlet, so a draw takes a fixed
// split (OutletSplit) from each and the tap gets the mix. The system's
// usable_l40 is a draw simulated at that split (estimate.Merged), and
// usable_l40_low that × the estimate's LowFactor. One tank taking most of
// the flow and running cold first brings the tap below MinC whatever the
// other holds (quiet-night's morning: the right tank's top at 35.8 °C, the
// left's at 63.1, the first litre 38.8 °C and nothing usable, against 233 L
// summed). The split is assumed fixed by the pipework; whether buoyancy
// shifts it towards a hotter tank is open (no fixture has a draw with one
// tank cold).
//
// low_hot_water is judged for the system, never one tank, on its
// usable_l40_low, with hysteresis: it sets below LowHotWaterL, clears above
// LowHotWaterL + LowHotWaterMarginL, and holds between. It is re-judged at
// the end of every input and Tick. While the totals are unknown it is nil,
// and the held value is kept, so when they are known again a value inside
// the margin keeps what was set before (the first judgement, with nothing
// held, is simply below LowHotWaterL).
//
// TODO(phase 2 follow-up): top_c is the estimate's profile top. PLAN.md
// wants the outlet probe's last draw peak, decayed; the seam is the outlet
// probe's reading before the profile is built (package profile).
package engine
