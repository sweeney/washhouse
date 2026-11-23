package washhouse_test

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"github.com/sweeney/common/wire/washhouse"
)

// Every example in docs/WIRE.md is a message common/wire decodes and
// re-encodes to exactly the same bytes, once compacted: the document
// can't drift from the code that speaks it. (common/wire's goldens are
// these examples too.)
func TestWireDocExamplesRoundTrip(t *testing.T) {
	doc, err := os.ReadFile("docs/WIRE.md")
	if err != nil {
		t.Fatal(err)
	}
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
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Error(err)
	}
}
