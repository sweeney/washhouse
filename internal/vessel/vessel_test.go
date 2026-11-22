package vessel

import (
	"math"
	"strings"
	"testing"
)

func TestSTI210Validates(t *testing.T) {
	if err := STI210().Validate(); err != nil {
		t.Fatalf("STI210().Validate() = %v", err)
	}
}

func TestValidateRefusesBadParams(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Params)
		want string
	}{
		{"zero capacity", func(p *Params) { p.CapacityL = 0 }, "capacity"},
		{"negative diameter", func(p *Params) { p.InnerDiameterMM = -1 }, "inner diameter"},
		{"zero dome", func(p *Params) { p.DomeDepthMM = 0 }, "dome depth"},
		{"dome deeper than radius", func(p *Params) { p.DomeDepthMM = 231 }, "dome depth"},
		{"negative z0", func(p *Params) { p.Z0MM = -1 }, "z0"},
		{"zero slice", func(p *Params) { p.SliceMM = 0 }, "slice"},
		{"NaN capacity", func(p *Params) { p.CapacityL = math.NaN() }, "capacity"},
		{"Inf z0", func(p *Params) { p.Z0MM = math.Inf(1) }, "z0"},
		{"domes hold more than capacity", func(p *Params) { p.CapacityL = 10 }, "capacity"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := STI210()
			c.mod(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate() = %q, want it to mention %q", err, c.want)
			}
			if _, err := New(p); err == nil {
				t.Fatal("New accepted params that Validate refuses")
			}
		})
	}
}

// The figures avail.py --geometry prints for the STI 210, to its precision.
// Exact parity with avail.py is asserted in internal/estimate's parity test.
func TestSTI210Summary(t *testing.T) {
	v, err := New(STI210())
	if err != nil {
		t.Fatal(err)
	}
	s := v.Summary()
	near := func(name string, got, want, tol float64) {
		t.Helper()
		if math.Abs(got-want) > tol {
			t.Errorf("%s = %v, want %v ± %v", name, got, want, tol)
		}
	}
	near("DomeL", s.DomeL, 5.1, 0.05)
	near("StraightMM", s.StraightMM, 1142, 0.5)
	near("LitresPerCM", s.LitresPerCM, 1.66, 0.005)
	near("BottomMM", s.BottomMM, 150, 0)
	near("TopMM", s.TopMM, 1412, 0.5)
	near("TotalL", s.TotalL, 200, 0.05)
	near("straight + domes", s.StraightMM+2*60, s.TopMM-s.BottomMM, 1e-9)
}

func TestSlicesCoverTheWaterAtSliceSteps(t *testing.T) {
	v, err := New(STI210())
	if err != nil {
		t.Fatal(err)
	}
	sl := v.Slices()
	if got, want := len(sl), 1263; got != want { // floor(150) .. floor(1412.08), 1 mm each
		t.Fatalf("len(Slices()) = %d, want %d", got, want)
	}
	if sl[0].HeightMM != 150.5 || sl[len(sl)-1].HeightMM != 1412.5 {
		t.Fatalf("slices run %v..%v mm, want 150.5..1412.5", sl[0].HeightMM, sl[len(sl)-1].HeightMM)
	}
	for i := 1; i < len(sl); i++ {
		if d := sl[i].HeightMM - sl[i-1].HeightMM; d != 1 {
			t.Fatalf("slice %d is %v mm above the last, want 1", i, d)
		}
	}
	// The slice above the top of the water (its centre is past 1412.08 mm) holds none.
	if sl[len(sl)-1].Litres != 0 {
		t.Errorf("top slice holds %v L, want 0", sl[len(sl)-1].Litres)
	}
	// A straight-wall slice holds pi * 230^2 mm^2 * 1 mm.
	mid := sl[600]
	if want := math.Pi * 230 * 230 / 1e6; math.Abs(mid.Litres-want) > 1e-15 {
		t.Errorf("straight slice at %v mm holds %v L, want %v", mid.HeightMM, mid.Litres, want)
	}
	// Domes are thinner at the very bottom.
	if !(sl[0].Litres < sl[30].Litres && sl[30].Litres < sl[59].Litres && sl[59].Litres < mid.Litres) {
		t.Errorf("bottom dome slices don't grow towards the straight wall: %v %v %v %v",
			sl[0].Litres, sl[30].Litres, sl[59].Litres, mid.Litres)
	}
}

func TestCoarserSlicesHoldTheSameWater(t *testing.T) {
	p := STI210()
	p.SliceMM = 5
	v, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Summary().TotalL; math.Abs(got-200) > 0.5 {
		t.Errorf("5 mm slices hold %v L, want about 200", got)
	}
	sl := v.Slices()
	if d := sl[1].HeightMM - sl[0].HeightMM; d != 5 {
		t.Errorf("5 mm slices are %v mm apart", d)
	}
}

func TestLitresBetween(t *testing.T) {
	v, err := New(STI210())
	if err != nil {
		t.Fatal(err)
	}
	// avail.py --geometry: 400–825 mm: 70.6 L (all straight wall).
	if got := v.LitresBetween(400, 825); math.Abs(got-70.6) > 0.05 {
		t.Errorf("LitresBetween(400, 825) = %v, want 70.6", got)
	}
	if got := v.LitresBetween(0, 2000); math.Abs(got-v.Summary().TotalL) > 1e-9 {
		t.Errorf("LitresBetween(all) = %v, want TotalL %v", got, v.Summary().TotalL)
	}
}

func TestSlicesIsACopy(t *testing.T) {
	v, err := New(STI210())
	if err != nil {
		t.Fatal(err)
	}
	v.Slices()[0].Litres = 99
	if v.Slices()[0].Litres == 99 {
		t.Error("Slices() exposes the vessel's own slice")
	}
}
