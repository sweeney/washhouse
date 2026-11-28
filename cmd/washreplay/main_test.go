package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files from this run")

const (
	fixture     = "../../testdata/fixtures/shower-layered.log.gz"
	examplePath = "../../testdata/behaviour.example.json"
)

func runOK(t *testing.T, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := run(args, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut.String())
	}
	return out.String()
}

// golden compares got with testdata/name, or rewrites it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./cmd/washreplay -update)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rerun with -update if the change is meant):\n%s", path, diff(string(want), got))
	}
}

// diff is the first lines that differ.
func diff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	n := 0
	for i := 0; i < max(len(w), len(g)) && n < 5; i++ {
		var a, c string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			c = g[i]
		}
		if a != c {
			n++
			b.WriteString("line " + strconv.Itoa(i+1) + "\n want " + a + "\n  got " + c + "\n")
		}
	}
	return b.String()
}

func TestGoldenText(t *testing.T) {
	got := runOK(t, "-config", examplePath, "-state-every", "10m", fixture)
	golden(t, "shower-layered.txt", got)
	for _, want := range []string{" draw_started ", " draw_ended ", " draw_balanced ", "kind=shower", " state "} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in the output", want)
		}
	}
}

func TestGoldenJSON(t *testing.T) {
	got := runOK(t, "-config", examplePath, "-state-every", "10m", "-json", fixture)
	golden(t, "shower-layered.jsonl", got)
	sc := bufio.NewScanner(strings.NewReader(got))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	types := map[string]int{}
	endedAt := map[string]string{}
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not a JSON line: %v: %s", err, sc.Text())
		}
		ty, _ := m["type"].(string)
		types[ty]++
		switch ty {
		case "draw_ended":
			d := m["draw_ended"].(map[string]any)
			for _, k := range []string{"tanks", "outlet_peak_c", "heat_balance_l", "heat_balance_reliable", "reasons", "masked_by_reheat"} {
				if _, ok := d[k]; ok {
					t.Errorf("draw_ended carries %q: the balance fields must be absent", k)
				}
			}
			endedAt[d["draw_id"].(string)] = d["ended_at"].(string)
		case "draw_balanced":
			d := m["draw_balanced"].(map[string]any)
			id := d["draw_id"].(string)
			if ea, ok := endedAt[id]; !ok || ea != d["ended_at"] {
				t.Errorf("draw_balanced %s ended_at %v, draw_ended's %q (%v)", id, d["ended_at"], ea, ok)
			}
		}
		if _, ok := m["at"].(string); !ok {
			t.Errorf("line without an at: %s", sc.Text())
		}
	}
	// The 6 L, the 50 L shower and the 15 L; the lone litre at 0:34 is creep.
	if types["draw_ended"] != 3 || types["draw_balanced"] != 3 || types["state"] == 0 {
		t.Errorf("types %v, want 3 draw_ended, 3 draw_balanced and some states", types)
	}
}

// A plain recording reads the same as its gzipped copy.
func TestPlainAndGzipAgree(t *testing.T) {
	f, err := os.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if _, err := plain.ReadFrom(z); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "shower-layered.log")
	if err := os.WriteFile(path, plain.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	a := runOK(t, "-config", examplePath, fixture)
	b := runOK(t, "-config", examplePath, path)
	if a != b || a == "" {
		t.Errorf("gzip and plain differ (or are empty):\n%s", diff(a, b))
	}
}

// State lines are off with -state-every 0, except the last.
func TestStateEveryZero(t *testing.T) {
	got := runOK(t, "-config", examplePath, "-state-every", "0", fixture)
	if n := strings.Count(got, " state "); n != 1 {
		t.Errorf("%d state lines, want only the final one", n)
	}
}

func TestUsageErrors(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"colour":"blue"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no files", []string{"-config", examplePath}, 2, "FILE"},
		{"no config", []string{fixture}, 2, "-config"},
		{"unknown flag", []string{"-nope", fixture}, 2, "nope"},
		{"bad config", []string{"-config", bad, fixture}, 1, "colour"},
		{"missing config", []string{"-config", "/nonexistent.json", fixture}, 1, "nonexistent"},
		{"missing recording", []string{"-config", examplePath, "/nonexistent.log.gz"}, 1, "nonexistent"},
		{"negative state-every", []string{"-config", examplePath, "-state-every", "-1m", fixture}, 2, "state-every"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(tc.args, &out, &errOut); code != tc.code || !strings.Contains(errOut.String(), tc.want) {
				t.Errorf("exit %d, stderr %q; want %d mentioning %q", code, errOut.String(), tc.code, tc.want)
			}
		})
	}
}

// A blind tank's refused sweeps aren't printed one by one (right-bus-lost:
// 7,439 of them); its fault and tank_lost say it, as in the service.
func TestBlindSweepsAreQuiet(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-config", examplePath, "-wire", "../../testdata/fixtures/right-bus-lost.log.gz"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if n := strings.Count(errOut.String(), "\n"); n != 0 {
		t.Errorf("%d lines on stderr, want none:\n%.400s", n, errOut.String())
	}
	if !strings.Contains(out.String(), `"event":"tank_lost"`) {
		t.Error("no tank_lost on the wire")
	}
}
