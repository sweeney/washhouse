package profile

import (
	"errors"
	"math"
	"testing"
)

func temp(t float64) *float64 { return &t }

func probes(hts []float64, temps ...*float64) []Probe {
	ps := make([]Probe, len(hts))
	for i, h := range hts {
		ps[i] = Probe{HeightMM: h, TempC: temps[i]}
	}
	return ps
}

func mustNew(t *testing.T, ps []Probe, o Options) *Profile {
	t.Helper()
	p, err := New(ps, o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func checkAt(t *testing.T, p *Profile, z, want float64) {
	t.Helper()
	if got := p.At(z); math.Abs(got-want) > 1e-12 {
		t.Errorf("At(%v) = %v, want %v", z, got, want)
	}
}

func TestLinearBetweenFlatBeyond(t *testing.T) {
	p := mustNew(t, probes([]float64{100, 200, 400}, temp(10), temp(20), temp(60)), Options{})
	checkAt(t, p, 0, 10)
	checkAt(t, p, 100, 10)
	checkAt(t, p, 150, 15)
	checkAt(t, p, 200, 20)
	checkAt(t, p, 300, 40)
	checkAt(t, p, 400, 60)
	checkAt(t, p, 9999, 60)
}

func TestStratIsTheDefaultAndLiftsADip(t *testing.T) {
	ps := probes([]float64{100, 200, 300}, temp(50), temp(40), temp(60))
	strat := mustNew(t, ps, Options{})
	checkAt(t, strat, 200, 50)
	checkAt(t, strat, 250, 55)
	raw := mustNew(t, ps, Options{Raw: true})
	checkAt(t, raw, 200, 40)
	checkAt(t, raw, 250, 50)
}

func TestStratLiftsAColdTop(t *testing.T) {
	// Probe 5 heat-sunk by the outlet pipe reads below the water under it.
	ps := probes([]float64{100, 200}, temp(60), temp(49))
	checkAt(t, mustNew(t, ps, Options{}), 300, 60)
	checkAt(t, mustNew(t, ps, Options{Raw: true}), 300, 49)
}

func TestUnknownProbeIsSkipped(t *testing.T) {
	p := mustNew(t, probes([]float64{100, 200, 300}, temp(10), nil, temp(30)), Options{})
	checkAt(t, p, 200, 20)
	if got := len(p.Points()); got != 2 {
		t.Errorf("Points() has %d points, want 2", got)
	}
}

func TestPointsAreTheEffectiveProfile(t *testing.T) {
	p := mustNew(t, probes([]float64{100, 200, 300}, temp(50), temp(40), temp(60)), Options{})
	want := []Point{{100, 50}, {200, 50}, {300, 60}}
	got := p.Points()
	if len(got) != len(want) {
		t.Fatalf("Points() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Points() = %v, want %v", got, want)
		}
	}
	got[0].TempC = -1
	if p.Points()[0].TempC == -1 {
		t.Error("Points() exposes the profile's own slice")
	}
}

func TestTooFewProbes(t *testing.T) {
	for _, ps := range [][]Probe{
		nil,
		probes([]float64{100}, temp(10)),
		probes([]float64{100, 200, 300}, nil, temp(55), nil),
	} {
		if _, err := New(ps, Options{}); !errors.Is(err, ErrTooFewProbes) {
			t.Errorf("New(%d probes) err = %v, want ErrTooFewProbes", len(ps), err)
		}
	}
}

func TestBadInputIsRefused(t *testing.T) {
	cases := map[string][]Probe{
		"equal heights":        probes([]float64{100, 100}, temp(10), temp(20)),
		"descending heights":   probes([]float64{200, 100}, temp(10), temp(20)),
		"NaN height":           probes([]float64{math.NaN(), 100}, temp(10), temp(20)),
		"NaN temperature":      probes([]float64{100, 200}, temp(math.NaN()), temp(20)),
		"infinite temperature": probes([]float64{100, 200}, temp(10), temp(math.Inf(1))),
		"unknown role":         {{HeightMM: 100, TempC: temp(1), Role: "anode"}, {HeightMM: 200, TempC: temp(2)}},
	}
	for name, ps := range cases {
		if _, err := New(ps, Options{}); err == nil || errors.Is(err, ErrTooFewProbes) {
			t.Errorf("%s: New err = %v, want a validation error", name, err)
		}
	}
	// An unknown probe's height still has to be in order: it's a config fault.
	if _, err := New(probes([]float64{100, 50, 200}, temp(1), nil, temp(2)), Options{}); err == nil {
		t.Error("out-of-order unknown probe accepted")
	}
}

func TestRolesChangeNothingByDefault(t *testing.T) {
	hts := []float64{265, 400, 825, 945, 1215}
	ts := []*float64{temp(30), temp(55), temp(60), temp(48), temp(61)}
	plain := probes(hts, ts...)
	roled := probes(hts, ts...)
	for i, r := range []Role{RoleInlet, RoleBulk, RoleBulk, RoleReturn, RoleOutlet} {
		roled[i].Role = r
	}
	for _, o := range []Options{{}, {Raw: true}} {
		a, b := mustNew(t, plain, o), mustNew(t, roled, o)
		for z := 0.0; z < 1500; z += 7 {
			if a.At(z) != b.At(z) {
				t.Fatalf("%+v: roles changed At(%v): %v vs %v", o, z, a.At(z), b.At(z))
			}
		}
	}
}

func TestExcludeRolesIsOptIn(t *testing.T) {
	ps := probes([]float64{100, 200, 300}, temp(50), temp(40), temp(60))
	ps[1].Role = RoleReturn
	p := mustNew(t, ps, Options{Raw: true, Exclude: []Role{RoleReturn}})
	checkAt(t, p, 200, 55)

	ps[0].Role = RoleInlet
	ps[2].Role = RoleOutlet
	if _, err := New(ps, Options{Exclude: []Role{RoleInlet, RoleOutlet}}); !errors.Is(err, ErrTooFewProbes) {
		t.Errorf("excluding all but one probe: err = %v, want ErrTooFewProbes", err)
	}
	if _, err := New(ps, Options{Exclude: []Role{"anode"}}); err == nil {
		t.Error("unknown role in Exclude accepted")
	}
}

// A suspect reading (tanktemp's 85c_unverified: exactly 85 °C, the DS18B20's
// power-on value) is unknown unless the caller trusts suspects. Strat would
// otherwise spread a suspect 85 at the bottom up the whole tank.
func TestSuspectReadingIsUnknownByDefault(t *testing.T) {
	hts := []float64{265, 400, 825, 945, 1215}
	ps := probes(hts, temp(85), temp(40), temp(60), temp(60), temp(60))
	ps[0].Suspect = true
	unknown := probes(hts, nil, temp(40), temp(60), temp(60), temp(60))
	got, want := mustNew(t, ps, Options{}), mustNew(t, unknown, Options{})
	for _, z := range []float64{150, 265, 400, 600, 1412} {
		if a, b := got.At(z), want.At(z); a != b {
			t.Errorf("At(%v) = %v with the suspect probe, %v with it unknown", z, a, b)
		}
	}
	if trusted := mustNew(t, ps, Options{TrustSuspect: true}); trusted.At(265) != 85 {
		t.Errorf("TrustSuspect: At(265) = %v, want the 85 kept", trusted.At(265))
	}
}
