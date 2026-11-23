package washhouse_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sweeney/common/wire/washhouse"
)

// Every example in docs/WIRE.md is a message common/wire decodes and
// re-encodes to exactly the same bytes, once compacted, and is one of the
// goldens of the pinned common/wire module: the document can't drift from
// the code that speaks it, nor from the bytes that pin it.
func TestWireDocExamplesRoundTrip(t *testing.T) {
	doc, err := os.ReadFile("docs/WIRE.md")
	if err != nil {
		t.Fatal(err)
	}
	goldens := pinnedGoldens(t)
	blocks := regexp.MustCompile("(?s)```json\n(.*?)```").FindAllSubmatch(doc, -1)
	if len(blocks) < 8 {
		t.Fatalf("WIRE.md has %d json examples, want at least 8", len(blocks))
	}
	for _, m := range blocks {
		var c bytes.Buffer
		if err := json.Compact(&c, m[1]); err != nil {
			t.Errorf("an example isn't JSON: %v\n%s", err, m[1])
			continue
		}
		want := c.Bytes()
		var head struct{ Schema string }
		_ = json.Unmarshal(want, &head)
		var got []byte
		switch head.Schema {
		case washhouse.SchemaState:
			v, err := washhouse.DecodeState(want)
			if err == nil {
				got, err = washhouse.EncodeState(v)
			}
			check(t, err)
		case washhouse.SchemaTank:
			v, err := washhouse.DecodeTank(want)
			if err == nil {
				got, err = washhouse.EncodeTank(v)
			}
			check(t, err)
		case washhouse.SchemaEvent:
			v, err := washhouse.DecodeEvent(want)
			if err == nil {
				got, err = washhouse.EncodeEvent(v)
			}
			check(t, err)
		case washhouse.SchemaStatus:
			v, err := washhouse.DecodeStatus(want)
			if err == nil {
				got, err = washhouse.EncodeStatus(v)
			}
			check(t, err)
		default:
			t.Errorf("an example with no known schema:\n%s", want)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("example doesn't round-trip:\n doc %s\ngot %s", want, got)
		}
		if !goldens[string(want)] {
			t.Errorf("example is no golden of the pinned common/wire:\n%s", want)
		}
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Error(err)
	}
}

// pinnedGoldens reads common/wire/washhouse's goldens from the module
// go.mod pins (the module cache, offline).
func pinnedGoldens(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/sweeney/common/wire").Output()
	if err != nil {
		t.Fatalf("locating common/wire: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(strings.TrimSpace(string(out)), "washhouse", "testdata", "*.golden"))
	if len(files) < 8 {
		t.Fatalf("%d goldens in the pinned common/wire", len(files))
	}
	g := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		g[string(b)] = true
	}
	return g
}
