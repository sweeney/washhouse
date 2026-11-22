// Package draw attributes metered draws to the tanks. It does not detect
// draws: the meter does (package meter), and its litres are the volume.
// For each ended draw it works out which tank supplied it, the delivered
// temperature, the energy used, and a probe heat-balance volume that is
// only a cross-check of the meter's (PLAN.md, internal/draw).
//
// It is pure: no I/O, no goroutines, no wall clock. Time is event time,
// from the sweeps, the meter's events and Tick.
//
// # The arithmetic
//
// Tank k's litres are its heat lost over what a litre from its own outlet
// carries: lost_k / (c × (outlet_peak_k − T_cold)). Shares are those
// normalised over the tanks. The outlet peak is the outlet probe's highest
// reading during the draw: at rest that probe is heat-sunk by the copper
// and reads 3–8 °C low, and its peak during flow is the true top. The
// system's outlet temperature is the share-weighted peak, and the heat
// balance is the total heat lost over what a litre at that temperature
// carries. With every loss taken at one sweep that is the tanks' litres
// summed; here the total comes from a later sweep (below).
//
// # Which sweeps
//
// Before: each tank's last sweep before the first litre's closing. The
// probes lag by 1–43 s, so it is still the tank before the draw.
//
// After: two sweeps, because the fixtures show two lags pulling different
// ways after a big draw. In the 50 L layered shower:
//   - the right tank's lower probe (P2, 400 mm) keeps falling for ten
//     minutes after the last litre, from 54 to 20 °C, so the total heat lost
//     reads short at first: the balance is −26% one minute after, −12% at 5
//     min, −4% at 8 min, −2% at 10 min;
//   - meanwhile the left tank's inlet probe warms back (its fitting
//     recovers), so its loss falls from 0.56 to 0.33 kWh while the right's
//     grows from 1.72 to 2.6. The left recovering while the right keeps
//     falling is heat moving between and within the tanks, not the draw,
//     and it pushes the right's share from 0.76 at one minute to 0.89 at
//     five or more.
//
// So the share sweep (ShareSettle, 60 s after the last litre: past the
// probes' lag, before the redistribution) gives the shares, each tank's
// LostKWh, outlet peak and inlet drop, and the balance sweep (BalanceSettle,
// 8 min) gives SettledLostKWh, EnergyKWh and the heat balance. At 60 s the
// share sweep is also about when the meter's draw_ended arrives. The
// outlet re-warming after a draw (+1–2 °C/min, the fitting, not heating)
// is outside the peak's window, which ends at the share sweep.
//
// A next draw starting before the balance sweep cuts it at the last sweep
// before its first litre, and the balance is unreliable (interrupted).
//
// # When the balance is unreliable
//
// Reasons lists why; Reliable is true when there are none.
//   - masked by a reheat: the programmer's HW was on at any moment from the
//     first before sweep to the last balance sweep, or a tank's stored
//     energy rose by more than GainKWh over that window. The cold inlet sits
//     in the coil zone, so a reheat warms what the draw brings in. (The
//     heat source is inferred from what the tanks do, never from CH.)
//   - small: under MinReliableLitres (20 L). On 1–6 L draws the inlet
//     probe's transient dip dominates.
//   - thermocline: at the before sweep, a tank's P2 below T_min and P3 at or
//     above it. A cold layer between them (about 57 L) is invisible to the
//     interpolated profile: a 15 L draw straight after a 50 L shower read as
//     4 L. A gap probe with no reading leaves this unchecked (gap_unread).
//   - a tank missing a before, share or balance sweep (too far from its
//     target, MaxSweepGap), the next draw interrupting, or no heat lost.
//
// A tank missing any sweep leaves every share unknown, not just its own:
// normalising over the others would hand them the whole draw. A tank
// without an outlet reading during the draw uses its before sweep's
// profile top instead, and says so (OutletFromProfile).
//
// Not modelled: standing loss over the window (about 0.05 kWh, a litre, in
// 8 min for the right tank on a loop night).
package draw
