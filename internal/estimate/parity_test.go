package estimate

// Parity with tanktemp's avail.py, the Python model this package ports.
// testdata/parity.json is written by testdata/gen_parity.py (see its header
// to regenerate); it holds avail.py's parameters, geometry, the text of
// `avail.py --geometry`, and assess() on synthetic profiles, raw and strat.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/vessel"
)

// relTol is the agreement required. Go follows avail.py's arithmetic term
// for term, so most figures agree bit for bit. The exception is the dome
// volume: Python's sum() (3.12+) is compensated, Go's loop is not, and the
// last-bit difference carries into the straight length, the top of the
// water and the top-dome slices. Measured (Python 3.14, darwin/arm64): the
// dome volume differs by 3.5e-16 relative and every other figure agrees bit
// for bit. 1e-12 leaves room for another platform without hiding a real
// change (a 1 mm slip in the slices moves the sums by ~1e-3).
const relTol = 1e-12

type parity struct {
	Params struct {
		CapacityL       float64   `json:"capacity_l"`
		InnerDiameterMM float64   `json:"inner_diameter_mm"`
		DomeDepthMM     float64   `json:"dome_depth_mm"`
		Z0MM            float64   `json:"z0_mm"`
		SliceMM         float64   `json:"slice_mm"`
		HeightsMM       []float64 `json:"heights_mm"`
		TColdC          float64   `json:"t_cold_c"`
		TMinC           float64   `json:"t_min_c"`
		TMixC           float64   `json:"t_mix_c"`
		KWhPerLitreK    float64   `json:"kwh_per_litre_k"`
	} `json:"params"`
	Geometry struct {
		DomeL       float64      `json:"dome_l"`
		CylAreaMM2  float64      `json:"cyl_area_mm2"`
		StraightMM  float64      `json:"straight_mm"`
		ZCyl0MM     float64      `json:"z_cyl0_mm"`
		ZCyl1MM     float64      `json:"z_cyl1_mm"`
		ZTopMM      float64      `json:"z_top_mm"`
		NSlices     int          `json:"n_slices"`
		FirstSlices [][2]float64 `json:"first_slices"`
		LastSlices  [][2]float64 `json:"last_slices"`
		SumL        float64      `json:"sum_l"`
		SumZL       float64      `json:"sum_z_l"`
		SumZ2L      float64      `json:"sum_z2_l"`
		Text        string       `json:"text"`
	} `json:"geometry"`
	Cases []struct {
		Name  string     `json:"name"`
		Temps []*float64 `json:"temps_c"`
		Raw   *result    `json:"raw"`
		Strat *result    `json:"strat"`
	} `json:"cases"`
}

type result struct {
	KWh   float64 `json:"kwh"`
	HotL  float64 `json:"hot_l"`
	Mix40 float64 `json:"mix40"`
	Mean  float64 `json:"mean"`
	Top   float64 `json:"top"`
}

func loadParity(t *testing.T) parity {
	t.Helper()
	b, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var p parity
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if err := dec.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if len(p.Cases) < 15 {
		t.Fatalf("parity.json has %d cases, want at least 15", len(p.Cases))
	}
	return p
}

// agree checks got against avail.py's want to relTol (absolute near zero).
func agree(t *testing.T, name string, got, want float64) {
	t.Helper()
	if d := math.Abs(got - want); d > relTol*math.Max(math.Abs(got), math.Abs(want)) && d > 1e-15 {
		t.Errorf("%s = %.17g, avail.py %.17g (rel diff %.3g)", name, got, want, d/math.Abs(want))
	}
}

func parityVessel(t *testing.T, p parity) *vessel.Vessel {
	t.Helper()
	vp := vessel.Params{
		CapacityL:       p.Params.CapacityL,
		InnerDiameterMM: p.Params.InnerDiameterMM,
		DomeDepthMM:     p.Params.DomeDepthMM,
		Z0MM:            p.Params.Z0MM,
		SliceMM:         p.Params.SliceMM,
	}
	if vp != vessel.STI210() {
		t.Errorf("vessel.STI210() = %+v, avail.py uses %+v", vessel.STI210(), vp)
	}
	v, err := vessel.New(vp)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestGeometryParity(t *testing.T) {
	p := loadParity(t)
	v := parityVessel(t, p)
	s, g := v.Summary(), p.Geometry
	agree(t, "DomeL", s.DomeL, g.DomeL)
	agree(t, "StraightMM", s.StraightMM, g.StraightMM)
	agree(t, "LitresPerCM", s.LitresPerCM, g.CylAreaMM2/1e5)
	agree(t, "BottomMM", s.BottomMM, p.Params.Z0MM)
	agree(t, "TopMM", s.TopMM, g.ZTopMM)

	sl := v.Slices()
	if len(sl) != g.NSlices {
		t.Fatalf("%d slices, avail.py %d", len(sl), g.NSlices)
	}
	for i, w := range g.FirstSlices {
		agree(t, fmt.Sprintf("slice %d height", i), sl[i].HeightMM, w[0])
		agree(t, fmt.Sprintf("slice %d litres", i), sl[i].Litres, w[1])
	}
	for i, w := range g.LastSlices {
		j := len(sl) - len(g.LastSlices) + i
		agree(t, fmt.Sprintf("slice %d height", j), sl[j].HeightMM, w[0])
		agree(t, fmt.Sprintf("slice %d litres", j), sl[j].Litres, w[1])
	}
	var sumL, sumZL, sumZ2L float64
	for _, x := range sl {
		sumL += x.Litres
		sumZL += float64(x.HeightMM * x.Litres)
		sumZ2L += float64(float64(x.HeightMM*x.HeightMM) * x.Litres)
	}
	agree(t, "sum L", sumL, g.SumL)
	agree(t, "sum z·L", sumZL, g.SumZL)
	agree(t, "sum z²·L", sumZ2L, g.SumZ2L)
	agree(t, "TotalL", s.TotalL, g.SumL)
}

// TestGeometryTextParity rebuilds `avail.py --geometry` from the Go model.
// The bands are avail.py's own: the cold inlet (281), probes 2–4, and the
// hot outlet (1237).
func TestGeometryTextParity(t *testing.T) {
	p := loadParity(t)
	v := parityVessel(t, p)
	s, vp := v.Summary(), v.Params()
	var b strings.Builder
	fmt.Fprintf(&b, "inner D %.0f mm, dome %.0f mm (%.1f L each), straight %.0f mm at %.2f L/cm; water from %.0f to %.0f mm; total %.1f L\n",
		vp.InnerDiameterMM, vp.DomeDepthMM, s.DomeL, s.StraightMM, s.LitresPerCM, s.BottomMM, s.TopMM, s.TotalL)
	for _, band := range [][2]float64{{s.BottomMM, 281}, {281, 400}, {400, 825}, {825, 945}, {945, 1237}, {1237, s.TopMM}} {
		fmt.Fprintf(&b, "  %5.0f–%5.0f mm: %5.1f L\n", band[0], band[1], v.LitresBetween(band[0], band[1]))
	}
	if got := b.String(); got != p.Geometry.Text {
		t.Errorf("geometry text differs from avail.py --geometry\n--- go\n%s--- avail.py\n%s", got, p.Geometry.Text)
	}
}

func TestAssessParity(t *testing.T) {
	p := loadParity(t)
	v := parityVessel(t, p)
	e, err := New(v, Config{
		ColdC: p.Params.TColdC, MinC: p.Params.TMinC, MixC: p.Params.TMixC,
		KWhPerLitreK: p.Params.KWhPerLitreK, LowFactor: 0.85,
	})
	if err != nil {
		t.Fatal(err)
	}
	roles := []profile.Role{profile.RoleInlet, profile.RoleBulk, profile.RoleBulk, profile.RoleReturn, profile.RoleOutlet}
	for _, c := range p.Cases {
		for _, variant := range []struct {
			name string
			raw  bool
			want *result
		}{{"raw", true, c.Raw}, {"strat", false, c.Strat}} {
			t.Run(c.Name+"/"+variant.name, func(t *testing.T) {
				ps := make([]profile.Probe, len(c.Temps))
				for i, tc := range c.Temps {
					// The right tank's roles: by default they change nothing.
					ps[i] = profile.Probe{HeightMM: p.Params.HeightsMM[i], TempC: tc, Role: roles[i]}
				}
				prof, err := profile.New(ps, profile.Options{Raw: variant.raw})
				if variant.want == nil {
					if err != profile.ErrTooFewProbes {
						t.Fatalf("avail.py gives no estimate; profile.New err = %v, want ErrTooFewProbes", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				got, w := e.Assess(prof), variant.want
				agree(t, "KWh", got.KWh, w.KWh)
				agree(t, "HotL", got.HotL, w.HotL)
				agree(t, "UsableL40", got.UsableL40, w.Mix40)
				agree(t, "UsableL40Low", got.UsableL40Low, w.Mix40*0.85)
				agree(t, "MeanC", got.MeanC, w.Mean)
				agree(t, "TopC", got.TopC, w.Top)
			})
		}
	}
}
