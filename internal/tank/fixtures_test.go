package tank

import (
	"bufio"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sweeney/washhouse/internal/profile"
	"github.com/sweeney/washhouse/internal/wire/tanktemp"
)

// The house's two cylinders: five probes each at the same heights; the
// right tank's upper probe is on the secondary return.
func houseTanks() map[string]Config {
	probes := func(upper profile.Role) []ProbeConfig {
		return []ProbeConfig{
			{Position: "bottom", Role: profile.RoleInlet, HeightMM: 265},
			{Position: "lower", Role: profile.RoleBulk, HeightMM: 400},
			{Position: "middle", Role: profile.RoleBulk, HeightMM: 825},
			{Position: "upper", Role: upper, HeightMM: 945},
			{Position: "top", Role: profile.RoleOutlet, HeightMM: 1215},
		}
	}
	return map[string]Config{
		"hot_water_cylinder_left":  {DeviceID: "hot_water_cylinder_left", Probes: probes(profile.RoleBulk)},
		"hot_water_cylinder_right": {DeviceID: "hot_water_cylinder_right", Probes: probes(profile.RoleReturn)},
	}
}

// Every tank state in every recorded fixture makes a sweep with a sane
// estimate: the probe layout and roles fit the real data.
func TestEveryFixtureStateSweeps(t *testing.T) {
	tanks := map[string]*Tank{}
	for id, c := range houseTanks() {
		tanks[id] = mustTank(t, c)
	}
	paths, _ := filepath.Glob("../../testdata/fixtures/*.log.gz")
	if len(paths) == 0 {
		t.Fatal("no fixtures")
	}
	n := 0
	for _, p := range paths {
		fh, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		z, err := gzip.NewReader(fh)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(z)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			parts := strings.SplitN(sc.Text(), " ", 3)
			if len(parts) < 3 || !strings.HasSuffix(parts[1], "/state") ||
				!(strings.HasPrefix(parts[1], "water/hot/tanktemp/") || strings.HasPrefix(parts[1], "energy/hot_water/")) {
				continue
			}
			st, err := tanktemp.DecodeState([]byte(parts[2]))
			if err != nil {
				t.Fatalf("%s: %v", filepath.Base(p), err)
			}
			sw, err := tanks[st.DeviceID].Sweep(st)
			if err != nil {
				t.Fatalf("%s %s: %v", filepath.Base(p), st.Timestamp, err)
			}
			e := sw.Estimate
			if e.KWh < 0 || e.KWh > 13 || e.MeanC < 10 || e.MeanC > 75 || e.UsableL40 < 0 || e.UsableL40 > 400 {
				t.Errorf("%s %s %s: implausible estimate %+v", filepath.Base(p), st.DeviceID, st.Timestamp, e)
			}
			n++
		}
		fh.Close()
	}
	if n < 1000 {
		t.Errorf("only %d tank states in the fixtures", n)
	}
	t.Logf("%d tank states swept", n)
}
