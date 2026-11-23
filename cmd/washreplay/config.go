package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/sweeney/washhouse/internal/draw"
	"github.com/sweeney/washhouse/internal/engine"
	"github.com/sweeney/washhouse/internal/estimate"
	"github.com/sweeney/washhouse/internal/meter"
	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/tank"
	"github.com/sweeney/washhouse/internal/thermal"
	"github.com/sweeney/washhouse/internal/vessel"
)

// fileConfig is washreplay's config file (JSON). Every key is required,
// unknown keys and duplicate keys (ignoring case) are refused, an integer
// must be written as one, and a duration is a Go duration string ("60s").
// Values shared by several packages are given once: cold_c and the heat
// capacity (estimate and draw), and the tanks (their probes, the loss
// fit's priors, their share of the merged outlet, and the order every list
// uses).
type fileConfig struct {
	AmbientTopic           string       `json:"ambient_topic"`
	LowHotWaterL           float64      `json:"low_hot_water_l"`
	LowHotWaterMarginL     float64      `json:"low_hot_water_margin_l"`
	BoilerHeartbeat        duration     `json:"boiler_heartbeat"`
	BoilerMissedHeartbeats float64      `json:"boiler_missed_heartbeats"`
	RecentDraws            int          `json:"recent_draws"`
	ColdC                  float64      `json:"cold_c"`
	KWhPerLitreK           float64      `json:"heat_capacity_kwh_per_litre_k"`
	Vessel                 vesselFile   `json:"vessel"`
	Estimate               estimateFile `json:"estimate"`
	Tanks                  []tankFile   `json:"tanks"`
	Meter                  meterFile    `json:"meter"`
	Thermal                thermalFile  `json:"thermal"`
	Draw                   drawFile     `json:"draw"`
}

type vesselFile struct {
	CapacityL       float64 `json:"capacity_l"`
	InnerDiameterMM float64 `json:"inner_diameter_mm"`
	DomeDepthMM     float64 `json:"dome_depth_mm"`
	Z0MM            float64 `json:"z0_mm"`
	SliceMM         float64 `json:"slice_mm"`
}

type estimateFile struct {
	MinC       float64 `json:"min_c"`
	MixC       float64 `json:"mix_c"`
	LowFactor  float64 `json:"low_factor"`
	DrawFromMM float64 `json:"draw_from_mm"`
	DrawToMM   float64 `json:"draw_to_mm"`
}

type tankFile struct {
	DeviceID     string      `json:"device_id"`
	TrustSuspect bool        `json:"trust_suspect"`
	PriorUAWPerK float64     `json:"prior_ua_w_per_k"`
	PriorHours   float64     `json:"prior_hours"`
	OutletSplit  float64     `json:"outlet_split"`
	Probes       []probeFile `json:"probes"`
}

type probeFile struct {
	Position string  `json:"position"`
	Role     string  `json:"role"`
	HeightMM float64 `json:"height_mm"`
}

type meterFile struct {
	DeviceID             string   `json:"device_id"`
	DrawEnd              duration `json:"draw_end"`
	BathFlowLPerMin      float64  `json:"bath_flow_l_per_min"`
	BathMinLitres        float64  `json:"bath_min_litres"`
	ShowerMinLitres      float64  `json:"shower_min_litres"`
	ShowerMinDuration    duration `json:"shower_min_duration"`
	ShowerMinFlowLPerMin float64  `json:"shower_min_flow_l_per_min"`
	CreepClosed          duration `json:"creep_closed"`
	CreepWindow          duration `json:"creep_window"`
	StaleAfter           duration `json:"stale_after"`
	UpdateEvery          duration `json:"update_every"`
}

type thermalFile struct {
	PowerWindow     duration `json:"power_window"`
	PowerMinSpan    duration `json:"power_min_span"`
	PowerMinSamples int      `json:"power_min_samples"`
	PowerExclude    []string `json:"power_exclude"`
	MinProbes       int      `json:"min_probes"`
	StaleAfter      duration `json:"stale_after"`
	HeatingOnKW     float64  `json:"heating_on_kw"`
	HeatingOffKW    float64  `json:"heating_off_kw"`
	DrawSettle      duration `json:"draw_settle"`
	PowerSettle     duration `json:"power_settle"`
	SourceHold      duration `json:"source_hold"`
	UATau           duration `json:"ua_tau"`
	UAQuiet         duration `json:"ua_quiet"`
	UAMinDeltaK     float64  `json:"ua_min_delta_k"`
	AmbientStale    duration `json:"ambient_stale"`
}

type drawFile struct {
	ShareSettle       duration  `json:"share_settle"`
	BalanceSettle     duration  `json:"balance_settle"`
	MaxSweepGap       duration  `json:"max_sweep_gap"`
	MinReliableLitres float64   `json:"min_reliable_litres"`
	GainKWh           float64   `json:"gain_kwh"`
	ThermoclineC      float64   `json:"thermocline_c"`
	Gaps              []gapFile `json:"gaps"`
}

// gapFile is a thermocline gap: two probe positions, low below high.
type gapFile struct {
	Low  string `json:"low"`
	High string `json:"high"`
}

// duration is a time.Duration written as a Go duration string.
type duration time.Duration

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration must be a string such as \"60s\", got %s", b)
	}
	x, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = duration(x)
	return nil
}

// loadConfig decodes a config file strictly and builds the engine's
// config from it, validated.
func loadConfig(r io.Reader) (fileConfig, engine.Config, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return fileConfig{}, engine.Config{}, err
	}
	fc, err := decodeStrict(b)
	if err != nil {
		return fileConfig{}, engine.Config{}, fmt.Errorf("config: %w", err)
	}
	ec, err := fc.engine()
	if err != nil {
		return fileConfig{}, engine.Config{}, fmt.Errorf("config: %w", err)
	}
	return fc, ec, nil
}

func decodeStrict(b []byte) (fileConfig, error) {
	var generic any
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&generic); err != nil {
		return fileConfig{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fileConfig{}, errors.New("data after the config object")
	}
	if err := noDuplicates(json.NewDecoder(bytes.NewReader(b)), ""); err != nil {
		return fileConfig{}, err
	}
	if err := present(reflect.TypeOf(fileConfig{}), generic, ""); err != nil {
		return fileConfig{}, err
	}
	var fc fileConfig
	dec = json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		return fileConfig{}, err
	}
	return fc, nil
}

// noDuplicates walks one JSON value from dec and refuses an object with
// two keys equal ignoring case (encoding/json would take the last).
func noDuplicates(dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key := k.(string)
			if seen[strings.ToLower(key)] {
				return fmt.Errorf("duplicate key %q", join(path, key))
			}
			seen[strings.ToLower(key)] = true
			if err := noDuplicates(dec, join(path, key)); err != nil {
				return err
			}
		}
		_, err = dec.Token() // }
		return err
	case json.Delim('['):
		for i := 0; dec.More(); i++ {
			if err := noDuplicates(dec, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		_, err = dec.Token() // ]
		return err
	}
	return nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// present checks that v, decoded generically, has exactly the keys t's JSON
// tags name (spelt exactly), none of them null, and durations that parse,
// down through nested structs and slices of them.
func present(t reflect.Type, v any, path string) error {
	if t == reflect.TypeOf(duration(0)) {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: a duration must be a string such as \"60s\"", path)
		}
		if _, err := time.ParseDuration(s); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object", orTop(path))
		}
		known := map[string]bool{}
		for i := 0; i < t.NumField(); i++ {
			known[strings.Split(t.Field(i).Tag.Get("json"), ",")[0]] = true
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !known[k] {
				return fmt.Errorf("unknown key %q", join(path, k))
			}
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			key := strings.Split(f.Tag.Get("json"), ",")[0]
			x, ok := m[key]
			if !ok {
				return fmt.Errorf("missing key %q", join(path, key))
			}
			if x == nil {
				return fmt.Errorf("%s is null", join(path, key))
			}
			if err := present(f.Type, x, join(path, key)); err != nil {
				return err
			}
		}
	case reflect.Slice:
		xs, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: want a list", path)
		}
		for i, x := range xs {
			if x == nil {
				return fmt.Errorf("%s[%d] is null", path, i)
			}
			if err := present(t.Elem(), x, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func orTop(path string) string {
	if path == "" {
		return "the config"
	}
	return path
}

// engine builds and validates the engine's config.
func (fc fileConfig) engine() (engine.Config, error) {
	c := engine.Config{
		Meter: meter.Config{
			DeviceID:             fc.Meter.DeviceID,
			DrawEnd:              time.Duration(fc.Meter.DrawEnd),
			BathFlowLPerMin:      fc.Meter.BathFlowLPerMin,
			BathMinLitres:        fc.Meter.BathMinLitres,
			ShowerMinLitres:      fc.Meter.ShowerMinLitres,
			ShowerMinDuration:    time.Duration(fc.Meter.ShowerMinDuration),
			ShowerMinFlowLPerMin: fc.Meter.ShowerMinFlowLPerMin,
			CreepClosed:          time.Duration(fc.Meter.CreepClosed),
			CreepWindow:          time.Duration(fc.Meter.CreepWindow),
			StaleAfter:           time.Duration(fc.Meter.StaleAfter),
			UpdateEvery:          time.Duration(fc.Meter.UpdateEvery),
		},
		Vessel: vessel.Params{CapacityL: fc.Vessel.CapacityL, InnerDiameterMM: fc.Vessel.InnerDiameterMM,
			DomeDepthMM: fc.Vessel.DomeDepthMM, Z0MM: fc.Vessel.Z0MM, SliceMM: fc.Vessel.SliceMM},
		Estimate: estimate.Config{ColdC: fc.ColdC, MinC: fc.Estimate.MinC, MixC: fc.Estimate.MixC,
			KWhPerLitreK: fc.KWhPerLitreK, LowFactor: fc.Estimate.LowFactor,
			DrawFromMM: fc.Estimate.DrawFromMM, DrawToMM: fc.Estimate.DrawToMM},
		Thermal: thermal.Config{
			PowerWindow:     time.Duration(fc.Thermal.PowerWindow),
			PowerMinSpan:    time.Duration(fc.Thermal.PowerMinSpan),
			PowerMinSamples: fc.Thermal.PowerMinSamples,
			MinProbes:       fc.Thermal.MinProbes,
			StaleAfter:      time.Duration(fc.Thermal.StaleAfter),
			HeatingOnKW:     fc.Thermal.HeatingOnKW,
			HeatingOffKW:    fc.Thermal.HeatingOffKW,
			DrawSettle:      time.Duration(fc.Thermal.DrawSettle),
			PowerSettle:     time.Duration(fc.Thermal.PowerSettle),
			SourceHold:      time.Duration(fc.Thermal.SourceHold),
			UATau:           time.Duration(fc.Thermal.UATau),
			UAQuiet:         time.Duration(fc.Thermal.UAQuiet),
			UAMinDeltaK:     fc.Thermal.UAMinDeltaK,
			AmbientStale:    time.Duration(fc.Thermal.AmbientStale),
		},
		Draw: draw.Config{
			ColdC:             fc.ColdC,
			KWhPerLitreK:      fc.KWhPerLitreK,
			ShareSettle:       time.Duration(fc.Draw.ShareSettle),
			BalanceSettle:     time.Duration(fc.Draw.BalanceSettle),
			MaxSweepGap:       time.Duration(fc.Draw.MaxSweepGap),
			MinReliableLitres: fc.Draw.MinReliableLitres,
			GainKWh:           fc.Draw.GainKWh,
			ThermoclineC:      fc.Draw.ThermoclineC,
		},
		LowHotWaterL:           fc.LowHotWaterL,
		LowHotWaterMarginL:     fc.LowHotWaterMarginL,
		BoilerHeartbeat:        time.Duration(fc.BoilerHeartbeat),
		BoilerMissedHeartbeats: fc.BoilerMissedHeartbeats,
		RecentDraws:            fc.RecentDraws,
	}
	for _, g := range fc.Draw.Gaps {
		c.Draw.Gaps = append(c.Draw.Gaps, draw.Gap{Low: g.Low, High: g.High})
	}
	for _, r := range fc.Thermal.PowerExclude {
		c.Thermal.PowerExclude = append(c.Thermal.PowerExclude, profile.Role(r))
	}
	for _, t := range fc.Tanks {
		tc := tank.Config{DeviceID: t.DeviceID, TrustSuspect: t.TrustSuspect}
		for _, p := range t.Probes {
			tc.Probes = append(tc.Probes, tank.ProbeConfig{Position: p.Position, Role: profile.Role(p.Role), HeightMM: p.HeightMM})
		}
		c.Tanks = append(c.Tanks, tc)
		c.Thermal.Tanks = append(c.Thermal.Tanks, thermal.TankConfig{DeviceID: t.DeviceID, PriorUAWPerK: t.PriorUAWPerK, PriorHours: t.PriorHours})
		c.Draw.Tanks = append(c.Draw.Tanks, t.DeviceID)
		c.OutletSplit = append(c.OutletSplit, t.OutletSplit)
		c.Draw.OutletSplit = append(c.Draw.OutletSplit, t.OutletSplit)
	}
	return c, c.Validate()
}
