# Hot water cylinders: what we know

From the tanktemp session's briefing of 2026-11-13, brought into washhouse on
2026-11-22. **What changed since then is marked _Update (Nov 2026)_.** The
biggest change: a hot-water meter has counted every litre since 2026-11-21,
so draws are no longer read from the probes (§6). Times of day of individual
draws and the house's usual hot-water routine are left out on purpose: they
are an occupancy pattern.

Written by the Claude session working in the tanktemp repo, from
two weeks of recordings (2026-11-02 → 2026-11-13) and the owner's own
descriptions. **Confidence is marked on each point**:
- **measured**: seen repeatedly in the data;
- **owner said**: the owner's own description or confirmation;
- **inferred**: our best explanation, not proven;
- **guess**: an assumption made to get a number.

All times are UTC (= UK time in winter). Don't put hostnames, IPs or house
details in any public repo; the owner is strict about PII in committed files.

---

## 1. The hardware

### Two cylinders, side by side, in the ground-floor boiler room
- Device IDs `hot_water_cylinder_left` and `hot_water_cylinder_right`.
- The room is a small plant cupboard that holds the boiler and both cylinders.
- **Left tank:** an Ariston **Classico HE STI 210** (owner's docs). The right tank is assumed to be the same model:
  every model applies the same geometry to both (**guess**).
  - 200 L, 550 mm outside diameter, 1498 mm tall, enamelled steel.
  - **Two magnesium sacrificial anodes**, one top and one bottom.
  - Rated standing loss **1.97 kWh/24 h at 60 °C, about 82 W**. The coil is
    rated at 27.2 kW, heating 15→60 °C in 27 min.
- **Fittings, measured from the base** (from Ariston's drawing):
  | Letter | Height | What it is |
  |---|---|---|
  | H | 241 mm | coil return |
  | G | 281 mm | cold inlet (with a spreader inside) |
  | — | ~380 mm | immersion boss (read off the drawing's scale, **guess**) |
  | D | 781 mm | coil flow (so the coil spans about **241–781 mm**) |
  | A | 956 mm | **secondary return** |
  | B | 1237 mm | hot outlet: the fitting. Inside, a pipe arches up to the very top of the dome (**owner said**, 24 Nov 2026), so a draw takes the hottest water first |

### Probes
- **Five DS18B20 probes per tank**, all at the same heights on each tank. They
  are numbered 1–5 from the bottom. The left tank is on 1-Wire bus D1, the
  right on D2, both read by a Raspberry Pi running **tanktemp**.
- **Each probe sits on a fitting, not on bare cylinder wall.** That matters
  for reading them:
  | # | Height | Position | Fitting it's on | How to read it |
  |---|---|---|---|---|
  | 1 | 265 mm | bottom | **cold inlet** (G) | the draw detector: drops sharply (−15 to −19 °C) on a big draw, as cold mains enters |
  | 2 | 400 mm | lower | looks like an anode boss / immersion boss (**owner said** "looks like") | inside the coil zone |
  | 3 | 825 mm | middle | looks like an anode boss | just above the top of the coil; the best "bulk hot water" probe |
  | 4 | 945 mm | upper | **blanked off on the left; on the right it is the secondary return (A)** | left: a normal tank probe. Right: reads the *return water*, not the tank (see §3) |
  | 5 | 1215 mm | top | **hot outlet** (B) | reads LOW at rest (see below); its peak during a draw is the true top temperature |
- **Probe 5 is heat-sunk by the copper outlet pipe** (**measured**). When a
  tap runs, hot water flows through the fitting and probe 5 reads the true top
  temperature, about 63 °C. Once the flow stops, the copper conducts heat away
  faster than the tank can feed it back through the wall. The probe falls
  quickly, for example 62.9 → 48.8 °C in 3 minutes, and settles several °C
  below the water beneath it. So **at rest, probe 5 under-reads the top of the
  tank**, by an amount that depends on how long it has been since the last
  draw. Never treat probe 5's resting value as "the top of the tank".
- **Accuracy:** the probes are uncalibrated (offset 0), so ±0.5 °C absolute.
  A probe reads 85 °C, or reports parasite power, for a few seconds after it
  is plugged in; that's normal. About 50 single read retries a day happen
  across both buses, and all recover on the second attempt.

---

## 2. How it's plumbed (owner said, 2026-11-05; "I think" in places)

- **The primary (boiler) side runs in parallel.** One boiler flow pipe feeds a
  manifold, and each tank's coil sits behind **its own valve**, believed to be
  driven by that tank's own cylinder stat. The coil returns merge back into one
  pipe.
- **The hot outlets merge.** Each tank's outlet (fitting B) joins into one pipe
  up to the house. The left outlet runs up and over the left tank, then drops
  to probe-5 height to join. The owner suspects a little backwash between the
  tanks at rest.
- _Update (Nov 2026):_ **the split between the tanks isn't fixed.** The owner's
  experience is that with one tank cold and the other hot, the taps still run
  hot. Tested on 24 Nov 2026 (**measured**): a 40 L run at 13 L/min with the
  left tank's outlet at 62 °C and the right's at 40 °C was too hot to hold
  throughout. The fixed 0.11/0.89 split (from the 50 L shower, both tanks hot)
  would have given 43 °C. Both inlets fell alike (−10 °C), so both tanks took
  water: about half each, or more from the hot one. So the hot tank supplies the
  tap, and washhouse counts the tanks' usable water summed.
- **Cold feed:** both bottoms (the probe-1 fittings) take cold water during a
  draw (**measured**). Both tanks usually supply a draw together.
- **Secondary return:** on the right tank only, fitting A (probe 4) is plumbed
  to what looks like a secondary return loop. **There is no pump anywhere in
  the house.** So whatever circulates does so by gravity (thermosiphon).
- **The boiler's HW/CH signals are only the programmer**, a smart switch that
  tells the boiler when to run. They know nothing about the tank stats or the
  valves (**owner said**). A tank whose stat is satisfied shows only in its
  probe data: its heat gain stops while HW is still ON.

---

## 3. The secondary return, and why the right tank loses heat 3–4× faster (history: 5–19 Nov)

> **Update (Nov 2026, measured; cause owner said):** the explanation below
> is confirmed. On 19 Nov the hot-water side was drained for an engineer's
> visit and an on/off valve was fitted on the secondary return next to the
> right cylinder (left open). Air left after the refill stopped the gravity
> loop: the right tank's overnight loss fell from 580–810 W to about 90 W
> (≈5 W/K, like the left), and its probe-4 offset went from −5.6 to about
> +2.5 °C. On 21 Nov a large draw from the loop's high point purged the air
> and the loop restarted: 369 W and −5.5 °C that night. The loss came back
> with a new primary valve in place (§4), which rules out gravity flow
> through a stuck-open primary: **the secondary return is the cause.** The
> owner plans to close the return valve for a few days in late November to
> measure the difference. So the loss is not a constant: washhouse fits UA
> per tank online.

**Measured, every night since 2026-11-05:**
- **Left tank standing loss: about 100–160 W**, roughly **3.2–4 W/K** against
  room temperature. That's 1.5–2× the 82 W rating; probe 5's outlet pipe and
  the pipework generally are the likely extra.
- **Right tank standing loss: about 350–455 W**, roughly **15–19 W/K**. That's
  4–5× the rating.
- **The right tank cools from the top and middle, almost uniformly**: about
  2–3 °C/h at the top and middle, about 1 °C/h at the bottom. For example, on
  the night of 2026-11-05/06, the right top fell 23 °C in 8 h while the left
  top fell 3 °C.
- **Right probe 4 (on the return fitting) sits 5–9 °C below its neighbours all
  night**: R4 − mean(R3, R5) = −5.2 to −5.8 °C. On the left tank, where that
  fitting is blanked, probe 4 sits slightly *above* the interpolation, by +1.2
  to +1.7 °C.
- The left tank gains nothing overnight, so the heat isn't crossing from one
  tank to the other.

**Our explanation (inferred, strongly supported):** the pumpless secondary
return **circulates by gravity 24 hours a day**. Hot water rises out of the
outlet into the house pipework, cools along the loop, and sinks back into the
right tank at fitting A (956 mm). That steadily mixes cool water into the
upper tank. It's the right tank's dominant heat loss, and it heats the house
pipework all night.
- **A competing explanation** is gravity circulation back through an open
  primary coil. We think it's less likely, because that would cool the tank
  from the bottom (where the coil is), and the data shows the top and middle
  cooling.
- **Fixes, in rough order of value** (none done yet):
  - lag the return loop's pipes;
  - fit a non-return valve, or isolate the loop;
  - add a timer-controlled pump so the loop runs only when it's wanted;
  - run on one tank (valve the right one off).
  The planner's what-ifs (§7) say cutting standby loss saves far more gas than
  any change of timer.

**A side effect:** on the right tank, probe 4 reads the return water, and at
times probe 5 reads below probe 4. Any "tank temperature" model must not take
right probe 4 at face value; see the stratification correction in §6.

---

## 4. The right tank is heated by central heating alone (history: before 19 Nov)

> **Update (Nov 2026, measured; cause owner said):** the right tank's coil
> valve was dead, and the engineer replaced it on 19 Nov. Before, CH-only
> spells heated the right tank at +0.7 to +6.4 kW; five CH-only spells from
> 19 to 22 Nov put about 0 kW in. So the inferred "valve open on CH calls"
> below was right, and is now fixed. washhouse must not assume CH heats
> either tank; it infers the heat source from observed gain.

- **Observed at least five times:** with **HW OFF and CH ON**, the right
  tank's coil zone heats and the left stays flat. For example:
  - **2026-11-06, about an hour:** right tank 7.6 → 12.8 kWh stored, coil
    zone 37 → 67 °C; the left stayed flat.
  - **2026-11-07, an hour's CH-only test** heated the right bottom from 17
    to 41 °C.
  - **2026-11-09:** with HW and CH both on, the right tank held 3–6 kW
    while the left fell to about 1.6 kW.
- **Size of it:** about **6 kWh a day**, roughly a third of all the heat going
  into the cylinders. That's free top-up in winter, and it disappears in
  summer when CH is off.
- **Why (inferred):** the right tank's coil valve is open on CH calls. It may
  be latched, have its manual lever set to open, or be miswired; for example,
  its valve or stat may be wired into the CH circuit rather than only the HW
  one. **Suggested check:** the right valve's manual lever, and its wiring.
  Not yet done.
- **When HW and CH run together**, the radiators take most of the boiler's
  output. Tank gain collapses to about 0.5–2 kW, and recovers to 3–6 kW when a
  draw cools the coils. A gain that "stops" while CH is on is usually this,
  not a stat closing.
- **The colder tank takes the larger share** of a reheat. With both valves
  open, the right (colder) coil took about 67–75 % in a HW-only test, because
  it has the bigger temperature difference to work with.

---

## 5. Data sources (MQTT and HTTP)

- **tanktemp:** retained, QoS 0, on `water/hot/tanktemp/{device_id}/state`
  (_Update:_ `energy/hot_water/...` until 2026-11-20). Each
  message carries every probe, bottom first, with `temp_c` (`null` = unknown,
  never 0), `raw_c`, `offset_c`, `error`, `suspect`, `rom`, `height_mm`,
  `order` and `position`.
  - Status and heartbeat are on `water/hot/tanktemp/status` (retained, QoS 1,
    with a last will).
  - **_Update:_ the hot-water meter** is on `water/hot/tanktemp/meter`: a reed
    meter on the merged hot outlet, 1 L per pulse, live since 2026-11-21. The
    payload carries a running `count` per counting run (`counting_since`,
    `seq`), `last_interval_ms` and `last_closed_ms`. tanktemp's `docs/METER.md`
    and `docs/WIRE.md` define it.
  - The **cadence is adaptive**: one sweep every 30 s, dropping to every 10 s
    while any probe changes at ≥1.5 °C/min, held 2 min, decaying over 5 min.
    So **10 s spells mean a draw or a reheat is happening**.
  - The contract is `docs/WIRE.md` in the tanktemp repo; the goldens there are
    normative.
- **Boiler programmer (boiler-sensor):**
  - `energy/boiler/sensor/system`: retained HEARTBEAT every 15 min, with
    `ch` and `hw` ON/OFF.
  - `energy/boiler/sensor/events`: HW_ON / HW_OFF / CH_ON / CH_OFF change
    events.
  - Remember these are the programmer's switches, not the stats or valves.
  - Changes land on the minute (within a second or so of :00).
  - _Update:_ after a reconnect, the retained `system` message can be a
    `RECONNECTED` event rather than a heartbeat.
- **statehouse** relays the programmer as devices on
  `house/state/devices/hot_water` and `.../central_heating`. Since 2026-11-13
  it also carries all ten probes as `appliance_probe` devices
  (`probe_hot_water_cylinder_{left,right}_{bottom,lower,middle,upper,top}`,
  identity scheme `tanktemp`, `part_of` the cylinder).
  - **The cylinders themselves are not in statehouse** (`telemetry: false`).
    The owner has a separate plan for them.
- **greenhouse** charts all ten probes, with history from **2026-11-11 about 12:00**.
  - `/devices/{id}/latest`
  - `/series?window=custom&from=…Z&to=…Z&interval={5m|15m|30m|1h|6h|1d}&shape=rows&devices=a,b`
- **Boiler-room climate sensor, new:** `climate_groundfloor_boiler-room`, in
  greenhouse **from 2026-11-13 about 09:30**, with `temperature_c` and
  `humidity_pct`.
  - It's coarse: readings step by about 1 °C, and some 15-min intervals are
    missing.
  - It climbed 14 → 22 °C over its first 3 hours, probably settling after
    install, so **ignore its first morning**.
  - One afternoon it read 21.2 °C while the hall (`climate_groundfloor`) read about
    16 °C. **The cupboard runs about 5 °C warmer than the hall**, warmed by
    the tanks and boiler.
  - Before it existed we used the hall as a stand-in for the room, so the
    earlier W/K figures above are computed against a too-cold room. **True
    W/K is somewhat higher** than quoted. Use this sensor for all loss work
    from 2026-11-13.
- **Recordings** (local only, never committed) in mosquitto_sub `%I %t %p`
  format: tanktemp's from 11-02 to 11-10 (`watch48b`, 11-08 → 10, has all 10
  probes), and washhouse's from 11-20 with the meter (`tools/record.sh`).
  Fixtures cut from them are shifted to a fake epoch.
  - tanktemp's analysis scripts (`avail.py`, `hwplan.py`, `ua.py`) are local
    too; washhouse ports `avail.py` with parity tests.

---

## 6. Estimating hot water available (`avail.py`)

- **Vessel model (all guesses past the outside dimensions):**
  - a vertical cylinder with shallow spherical domes top and bottom, holding
    exactly 200 L;
  - **inner diameter 460 mm** (550 mm outside, minus about 45 mm of insulation
    each side, **guess**);
  - **domes 60 mm deep** (**guess**);
  - **inner bottom 150 mm above the floor** (**guess**), chosen so that
    fittings H and G sit on the straight wall just above the bottom dome;
  - that gives a straight section of about 1142 mm at about 1.66 L/cm, with
    water from about 150 to 1412 mm.
- **Temperature profile:** linear between the five probe heights, and flat
  beyond the end probes.
- **The stratification correction ('strat'):** a still tank can't be colder
  above than below. So take a running maximum from the bottom up, which lifts
  probe 5 (heat-sunk) and right probe 4 (return water). The uncorrected 'raw'
  profile is a lower bound and is only 1–3 % lower.
- **Stored energy E:** heat above **10 °C** (assumed mains temperature) per
  slice, at 4.186 kJ/(L·K).
- **Usable water:** only water at **≥ 40 °C** counts. Each such litre is
  converted to **litres of 40 °C water when mixed with 10 °C cold** ("mix40").
  The model assumes plug flow, with the top water leaving first. Real draws
  mix the thermocline, so **expect about 85–90 % of mix40**.
- **A typical full state:** about 11–12 kWh and about 300–320 L mix40 per tank
  when hot (a mean of about 60 °C), so **about 550–630 L of 40 °C water for
  both tanks**. - _Update (measured by the meter):_ hot flows are taps 3.5–5 L/min, showers
  4–6.4 L/min (34–51 L each) and baths 12–31 L/min (72–87 L). A shower of about
  10.5 L/min at 40 °C takes about 6.3 L/min from the tank.

### Reading draws from the probes (history: superseded by the meter)

> **Update (Nov 2026, measured against the meter):** draws are now counted,
> not inferred. Against metered litres, the probe heat balance was within 8 %
> on a 50 L shower from a layered tank, 3–5× too high on 1–6 L draws, nearly
> blind to a 15 L draw straight after a shower (the cold layer sat between
> probes 2 and 3), and useless during a reheat, while the meter counted a
> 72 L bath during a reheat exactly. Probe dips with zero metered litres were
> seen at night. The right tank supplies about 75 % of each draw. washhouse
> uses the probes only to attribute a metered draw and as a cross-check.
- **A big draw** (shower, bath, full hot tap): probe 1 on the cold inlet drops
  15–19 °C within about 90 s, and probe 5 on the outlet jumps to the true top
  temperature.
- **A small draw** (basin, kitchen tap, kettle): probe 5 rises, and probe 1
  barely moves (−1 to −3.5 °C). **A heat balance can't see these**: one came
  out at about 2 L. Cold water settles below probe 1, so all volume estimates
  are **lower bounds**. (_Update:_ the meter replaces this.)
- **During a reheat, draws are masked.** The cold inlet (281 mm) sits inside
  the coil zone (241–781 mm), so the coil warms incoming water, and a shower
  during a reheat barely showed on probe 1. A heat balance during a reheat is useless
  for sizing draws.
- **After a draw**, probe 5 recovers at +1 to +2 °C/min for a few minutes.
  That's the fitting re-warming, not heating.
- A small nightly draw (10–20 L) was inferred from the probes.
  _Update:_ two metered nights showed **no such draw**.
- _Update (measured):_ the meter also shows **creep**: lone litres minutes
  apart with the reed held closed for minutes, about 0.07 L/min, over an
  evening. Cause unknown (a tap not fully closed, or circulation).

---

## 7. Dynamics and the hot water planner (`hwplan.py`)

- **The state** is total stored heat E in both tanks (kWh above 10 °C).
- **Idle loss is linear in E:** dE/dt = −(LOSS0 + LOSS1·E), fitted from
  draw-free stretches. About **0.5–1 kW for both tanks together**.
- **Reheat rate:**
  - HW only: about 10 kW in total at first, tapering. One measured run put
    3.4 kWh into one tank in 47 min, peaking at about 6.5 kW and falling to
    about 3 kW.
  - HW plus CH: about 5 kW.
  - A morning reheat (about an hour) puts in about 6–7.5 kWh.
  - "Full" is about 25 kWh for both tanks (the 98th percentile); that's where
    the stats close.
- **Usable litres** come from E by a least-squares fit, U ≈ K·(E − E0).
- **Draws** are extracted as the shortfall against the model.
- **Use (6 days, 2026-11-05 → 10):**
  - **130–430 L of 40 °C water a day**, in a morning and an evening peak
    (the evening one 120–250 L). The times are left out: occupancy pattern.
- **The energy budget:** the boiler puts about 18–20 kWh a day into the
  water. **About 13 kWh of that is standby loss**, and only 4–10 kWh is water
  actually used. **Loss dominates; timing barely matters.**
- **The programme:** two HW windows a day. Trimming each by about 15 min
  in winter saves about 1.4 kWh a day, about 6 %. Summer, with no free CH top-up, should keep the
  longer windows.
  - The tanks never fell below about 260 L usable on the old programme.
  - Every single-window programme ran out on heavy days, but only when the
    planner ignores CH-only charging (`--no-ch-charging`). With it counted
    (the default), a single midday window just holds 107–221 L.
  - **Correction (2026-11-13):** the recommended programme came from a
    one-off script that compared candidates close to the current programme,
    and that script is lost. `hwplan.py`'s own free search picks impractical
    windows (for example two half-hour windows, one before dawn). So treat the
    programme advice as reasoning about candidates, not as an optimum.
  - **Fitted constants** (rerun 2026-11-13 on two of tanktemp's recordings;
    _Update:_ the CH-only figure predates §4's change):
    - idle loss −0.54 + 0.063·E kW. The intercept is negative, which is
      unphysical below about 8.6 kWh; prefer a per-tank UA·(T − T_room)
      using the boiler-room sensor.
    - reheat: HW only 12.1 kW, HW+CH 5.3 kW, CH-only charging 2.5 kW
      averaged;
    - full ≈ 24.6 kWh;
    - usable ≈ 45·(E − 8.4) L at 40 °C.
  - The model is conservative: its predicted lows are 100–170 L below what
    actually happened.
  - **The trimmed programme hasn't been checked against real data yet.**
- **The planner's what-if switches:** `--loss-scale` (better lagging, <1) and
  a one-tank option (half the capacity and loss, reheat × 0.6, based on a
  **guessed** coil rate).

---

## 8. The plan to lag the pipes (agreed protocol, 2026-11-05; not yet done)

1. **The boiler-room climate sensor first.** Done on 2026-11-13. Then record
   **at least 3 normal baseline nights** with nothing else changed: the same
   stat settings, timer and tanktemp cadence.
2. **Lag the pipes, and note the time, which pipes, roughly how long each is,
   and which tank each belongs to:** hot outlet, cold feed, coil flow and
   return, and the secondary return.
3. **Record at least 3 nights after.**
4. **Compare:**
   - **The main measure:** loss per kelvin (W/K) between the tank and the
     cupboard air, over the draw-free overnight stretch: from about an hour
     after the last draw until the morning HW start (the meter now confirms
     no draw). W/K
     cancels out nights with different tank and room temperatures, including
     the cupboard cooling once lagging stops heating it.
   - **Second check: probe 5's resting gap** below the water under it should
     shrink if the outlet pipe is lagged. This shows on single nights. The
     same goes for probe 1 if the cold feed is lagged.
   - **The right tank in particular:** whether R4's gap of −5 °C closes, and
     whether its 15–19 W/K falls. That's what shows whether the return loop
     was the cause.
- **Expectation (guess):** a metre of bare 22 mm copper at 50–60 °C loses
  roughly 30–40 W. So a few metres of hot pipe and dead legs could account for
  the left tank's excess over its rating, and the gravity loop for most of the
  right tank's.

---

## 9. Open questions
- ~~The right tank's valve on CH calls (§4)~~ dead valve, replaced 19 Nov.
- ~~Gravity loop or open primary (§3)~~ the secondary return, confirmed when
  air stopped it 19–21 Nov.
- How much does closing the return valve save, and how much longer does the
  hot tap take to run hot at the loop's far end? (Planned late Nov, timed
  with the meter's litres.)
- Is the right tank really an STI 210?
- Inner diameter and insulation thickness (the vessel model's main guess).
- ~~Flow rates per outlet~~ measured by the meter (§6).
- ~~The nightly draw~~ not seen on metered nights (§6).
- What causes the evening creep (§6)?
- Water-bath calibration of the probes (offsets are 0 today).
- Does the trimmed HW programme hold up against real data?
