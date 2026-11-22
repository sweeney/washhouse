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
// carries. With the shares and the total from one sweep (the balance
// sweep, below) that is the tanks' litres summed.
//
// # Which sweeps
//
// Before: each tank's last sweep before the first litre's closing. The
// probes lag by 1–43 s, so it is still the tank before the draw.
//
// After: the probes keep moving for minutes after a big draw although no
// water moves once the last litre is in (standing loss is about 0.01 kWh a
// minute). In the 50 L layered shower:
//   - the right tank's lower probe (P2, 400 mm) keeps falling for ten
//     minutes after the last litre, from 54 to 20 °C, so the total heat lost
//     reads short at first: the balance is −26% one minute after, −12% at 5
//     min, −4% at 8 min, −2% at 10 min;
//   - the left tank's inlet probe warms back as its fitting recovers: its
//     cold layer (about 11 L, 70 mm above the inlet) sits wholly below P2,
//     so only the inlet probe saw it, and the profile painted a cold wedge
//     that fades. Its loss falls from 0.56 to 0.33 kWh while the right's
//     grows from 1.72 to 2.6.
//
// Both are the picture completing, not heat moving (nothing connects the
// tanks after the flow stops but the secondary loop and the shared
// manifolds, and neither moves 0.2 kWh in minutes). So the sweep whose
// total closes is the one to read the split from: at the balance sweep
// (BalanceSettle, 8 min) the right's share is 0.89, against 0.76 at one
// minute (tanktemp's review of PR #2; the bulk-probe energy at one minute,
// which leaves out the inlet fitting, also gives 0.89).
//
// So the balance sweep gives the shares, the tanks' litres, each tank's
// LostKWh, the energy used and the heat balance. The share sweep
// (ShareSettle, 60 s after the last litre, past the probes' lag) gives the
// outlet peaks and inlet drops (the outlet re-warming after a draw, +1–2
// °C/min from the fitting, is outside the peak's window, which ends
// there), EarlyLostKWh, and the fallback shares: a next draw starting
// before the balance sweep cuts it at the last sweep before its first
// litre (interrupted), and then, or when a tank has no balance sweep, the
// shares come from the share sweep (shares_early).
//
// The Attributor gives each draw in two stages: StageEnded once the meter
// has ended it and every tank's share sweep is in (about a minute after the
// last litre; the engine's draw_ended, which carries none of the
// attribution), and StageBalanced with the attribution at the balance
// sweep (draw_balanced).
//
// # When the balance is unreliable
//
// Reasons lists why; Reliable is true when there are none.
//   - masked by a reheat: the programmer's HW was on at any moment from the
//     first before sweep to the last balance sweep, or a tank's stored
//     energy rose by more than GainKWh over that window. The cold inlet sits
//     in the coil zone, so a reheat warms what the draw brings in. (The
//     heat source is inferred from what the tanks do, never from CH.)
//   - HW unknown (hw_unknown) at any moment of that window, including no
//     HW input at all: a reheat can't be ruled out. On the 87 L bath the
//     only sign of its reheat is hw_on (its bulk energy still fell), so
//     unknown can't count as off.
//   - small: under MinReliableLitres (20 L). On 1–6 L draws the inlet
//     probe's transient dip dominates.
//   - thermocline: at the before sweep, a tank's low gap probe below T_min
//     and its high one at or above it. Gaps names the pairs by probe
//     position (the house's: lower/middle, 400–825 mm, about 71 L in the
//     460 mm bore). A cold layer between them is invisible to the
//     interpolated profile: a 15 L draw straight after a 50 L shower read
//     as 4 L. A gap probe with no reading leaves this unchecked
//     (gap_unread).
//   - a tank missing a before, share or balance sweep (too far from its
//     target, MaxSweepGap), the next draw interrupting, the shares taken
//     early, or no heat lost.
//
// A masked draw (a reheat, or HW unknown) has no shares: a reheat heats
// whichever tank its coil is in, so the split would measure the coil, not
// the pipework (the 87 L bath "split" 0.57 left). Its losses, outlet peaks,
// energy and heat balance are still given (the outlet and the balance use
// the computed split; they are unreliable anyway). A tank missing any
// sweep leaves every share unknown, not just its own: normalising over the
// others would hand them the whole draw. A tank without an outlet reading
// during the draw uses its before sweep's profile top instead, and says so
// (OutletFromProfile).
//
// # Limitations
//
//   - The thermocline rule checks the before sweep only. A draw that ends
//     with its cold layer inside a gap has the same blur in its balance
//     sweep (probably less: P2 moves then). No fixture shows it yet.
//   - The upper/top pair (P4–P5, 270 mm, about 45 L) isn't configured: the
//     top probe is the heat-sunk outlet, which reads 3–8 °C low at rest, so
//     "top at or above T_min" can't be judged near 40 °C, and the right
//     tank's upper probe is the secondary return fitting, which reads about
//     5.6 K low while the loop runs, so the pair would flag the fitting
//     rather than a cold layer. Neither it nor middle/upper changes any
//     fixture draw's reasons. Gaps takes a list if one is wanted.
//   - Standing loss over the window is not modelled (about 0.05 kWh, a
//     litre, in 8 min for the right tank on a loop night).
//   - Below about 10 L the split scatters from 0 to 1: the inlet probe's
//     dip and the heat-sunk outlet warming to its true reading are as
//     large as the draw.
package draw
