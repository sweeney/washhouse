package estimate

import (
	"errors"
	"fmt"
	"math"

	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/vessel"
)

// Merged is what tanks in parallel deliver through one merged outlet.
type Merged struct {
	// FirstC is the first litre's temperature at the tap: the tanks' tops
	// mixed by the split.
	FirstC float64
	// Litres is how much can be drawn before the mix falls below MinC.
	Litres float64
	// UsableL40 is the litres of MixC water those Litres make with ColdC
	// water; UsableL40Low is that × LowFactor.
	UsableL40    float64
	UsableL40Low float64
}

// splitTolerance is how far a split's sum may be from 1.
const splitTolerance = 1e-9

// Merged simulates a draw through a merged outlet that takes the fixed
// fraction split[k] of every litre from tank k (profile ps[k]; every tank
// is this Estimator's vessel). Each tank is plug flow within its drawable
// band: after it has given v litres, its next litre is the water that was
// v litres below the top of the band (DrawToMM), and once the band is
// spent it gives mains water (ColdC). The tap gets the split's mix,
// Σ split[k]·T_k. The draw is followed until the mix falls below MinC; the
// litres up to there, mixed down to MixC, are UsableL40.
//
// With one tank this is the tank's own UsableL40 when its hot water is one
// layer at the top. With several it is less than their sum whenever one
// tank runs cold first: its cold water reaches the tap at its share of
// the flow, whatever the others hold.
func (e *Estimator) Merged(ps []*profile.Profile, split []float64) (Merged, error) {
	c := e.c
	if len(ps) == 0 || len(ps) != len(split) {
		return Merged{}, fmt.Errorf("estimate: %d profiles for %d split fractions", len(ps), len(split))
	}
	sum := 0.0
	for k, s := range split {
		if ps[k] == nil {
			return Merged{}, errors.New("estimate: a nil profile")
		}
		if math.IsNaN(s) || s < 0 || s > 1 {
			return Merged{}, fmt.Errorf("estimate: split %v must be in [0, 1]", s)
		}
		sum += s
	}
	if math.Abs(sum-1) > splitTolerance {
		return Merged{}, fmt.Errorf("estimate: split sums to %v, not 1", sum)
	}

	// Each tank's drawable column, top first.
	type layer struct{ litres, tempC float64 }
	var band []vessel.Slice
	e.v.Each(func(s vessel.Slice) {
		if c.DrawFromMM <= s.HeightMM && s.HeightMM < c.DrawToMM {
			band = append(band, s)
		}
	})
	cols := make([][]layer, len(ps))
	for k, p := range ps {
		col := make([]layer, 0, len(band))
		for i := len(band) - 1; i >= 0; i-- {
			col = append(col, layer{band[i].Litres, p.At(band[i].HeightMM)})
		}
		cols[k] = col
	}

	// Walk the draw from one layer boundary (in any tank) to the next: in
	// between, every tank gives one layer's water and the mix is constant.
	idx := make([]int, len(ps))
	left := make([]float64, len(ps)) // litres left in the current layer
	for k := range ps {
		if len(cols[k]) > 0 {
			left[k] = cols[k][0].litres
		}
	}
	var out Merged
	for first := true; ; first = false {
		mix, dv, next := 0.0, math.Inf(1), -1
		for k, s := range split {
			t := c.ColdC
			if idx[k] < len(cols[k]) {
				t = cols[k][idx[k]].tempC
				if s > 0 && left[k]/s < dv {
					dv, next = left[k]/s, k
				}
			}
			mix += s * t
		}
		if first {
			out.FirstC = mix
		}
		if mix < c.MinC || next < 0 {
			break
		}
		dv = max(dv, 0) // rounding, where two tanks' boundaries meet
		out.Litres += dv
		out.UsableL40 += dv * (mix - c.ColdC) / (c.MixC - c.ColdC)
		for k, s := range split {
			if k == next || idx[k] >= len(cols[k]) {
				continue
			}
			left[k] -= s * dv
		}
		idx[next]++
		if idx[next] < len(cols[next]) {
			left[next] = cols[next][idx[next]].litres
		}
	}
	out.UsableL40Low = out.UsableL40 * c.LowFactor
	return out, nil
}
