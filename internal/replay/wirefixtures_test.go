package replay_test

import (
	"bufio"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sweeney/common/wire/boiler"
	"github.com/sweeney/common/wire/tanktemp"
)

// Every message in every recorded fixture decodes with common/wire's
// decoders, independently of this package's reading of them.

// tanktempPrefixes are the production prefix and the one before 2026-11-20
// (WIRE.md, Changes); the recordings carry both.
var tanktempPrefixes = []string{"water/hot/tanktemp/", "energy/hot_water/"}

// decodeFixtureMessage decodes one recorded message with the decoder for its
// topic. known is false for a topic no decoder here reads (the boiler-room
// climate).
func decodeFixtureMessage(topic, payload string) (known bool, err error) {
	b := []byte(payload)
	for _, p := range tanktempPrefixes {
		rest, ok := strings.CutPrefix(topic, p)
		if !ok {
			continue
		}
		switch {
		case rest == "status":
			_, err = tanktemp.DecodeStatus(b)
		case rest == "meter":
			_, err = tanktemp.DecodeMeter(b)
		case strings.HasSuffix(rest, "/state") && strings.Count(rest, "/") == 1:
			_, err = tanktemp.DecodeState(b)
		default:
			return false, nil
		}
		return true, err
	}
	switch topic {
	case "energy/boiler/sensor/events":
		_, err = boiler.DecodeEvent(b)
		return true, err
	case "energy/boiler/sensor/system":
		_, err = boiler.DecodeSystem(b)
		return true, err
	}
	return false, nil
}

// Every tanktemp and boiler message in every recorded fixture decodes. A
// decoder stricter than what tanktemp (any build in the recordings) or
// boiler-sensor really sends fails here, not in production.
func TestEveryFixtureMessageDecodes(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "fixtures", "*.log.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures found")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			fh, err := os.Open(f)
			if err != nil {
				t.Fatal(err)
			}
			defer fh.Close()
			gz, err := gzip.NewReader(fh)
			if err != nil {
				t.Fatal(err)
			}
			sc := bufio.NewScanner(gz)
			sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
			decoded, failed := 0, 0
			for n := 1; sc.Scan(); n++ {
				parts := strings.SplitN(sc.Text(), " ", 3)
				if len(parts) != 3 {
					t.Errorf("line %d: not RECEIVE-TIME TOPIC PAYLOAD", n)
					continue
				}
				known, err := decodeFixtureMessage(parts[1], parts[2])
				if !known {
					if !strings.HasPrefix(parts[1], "zigbee2mqtt/") {
						t.Errorf("line %d: topic %s has no decoder", n, parts[1])
					}
					continue
				}
				decoded++
				if err != nil {
					failed++
					t.Errorf("line %d (%s): %v", n, parts[1], err)
				}
			}
			if err := sc.Err(); err != nil {
				t.Fatal(err)
			}
			if decoded == 0 {
				t.Error("no message decoded")
			}
			t.Logf("%d messages decoded, %d failed", decoded, failed)
		})
	}
}
