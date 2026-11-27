package behaviour

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// tools/config.html, the config explorer, describes every key of this
// document. Its sample must be testdata/behaviour.example.json, and its
// definitions must name exactly the keys washhouse decodes: one added here
// without a definition, or a definition left for a key that went, fails.
func TestConfigExplorerMatchesTheDecoder(t *testing.T) {
	page, err := os.ReadFile("../../tools/config.html")
	if err != nil {
		t.Fatal(err)
	}
	block := func(id string) any {
		m := regexp.MustCompile(`(?s)<script type="application/json" id="` + id + `">(.*?)</script>`).FindSubmatch(page)
		if m == nil {
			t.Fatalf("tools/config.html has no %q block", id)
		}
		var v any
		if err := json.Unmarshal(m[1], &v); err != nil {
			t.Fatalf("tools/config.html's %q block: %v", id, err)
		}
		return v
	}
	b, err := os.ReadFile("../../testdata/behaviour.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var example any
	if err := json.Unmarshal(b, &example); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(block("sample"), example) {
		t.Error("tools/config.html's sample isn't testdata/behaviour.example.json: copy it in")
	}
	// The example decodes strictly, every key present (Load), so its key
	// paths are the decoder's.
	want := map[string]bool{}
	var walk func(v any, path string)
	walk = func(v any, path string) {
		if path != "" {
			want[path] = true
		}
		switch x := v.(type) {
		case map[string]any:
			for k, y := range x {
				p := k
				if path != "" {
					p = path + "." + k
				}
				walk(y, p)
			}
		case []any:
			for _, y := range x {
				if _, ok := y.(map[string]any); ok {
					walk(y, path+"[]")
				}
			}
		}
	}
	walk(example, "")
	defs, ok := block("defs").(map[string]any)
	if !ok {
		t.Fatal("the defs block isn't an object")
	}
	var missing, extra []string
	for k := range want {
		if _, ok := defs[k]; !ok {
			missing = append(missing, k)
		}
	}
	types := map[string]bool{"number": true, "integer": true, "string": true, "duration": true, "boolean": true, "array": true, "object": true}
	for k, d := range defs {
		if !want[k] {
			extra = append(extra, k)
			continue
		}
		m, _ := d.(map[string]any)
		if s, _ := m["meaning"].(string); s == "" {
			t.Errorf("%s: no meaning", k)
		}
		if s, _ := m["type"].(string); !types[s] {
			t.Errorf("%s: type %q", k, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("keys washhouse decodes with no definition in tools/config.html: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("definitions in tools/config.html for keys washhouse doesn't decode: %v", extra)
	}
}

// corpusCase is one line of testdata/configtool/cases.jsonl: an edit to
// testdata/behaviour.example.json and whether washhouse accepts the
// result. tools/config_core_test.js asserts the explorer gives the same
// verdict on the same edits, so the page's rules can't drift from Load's.
type corpusCase struct {
	ID      string     `json:"id"`
	OK      *bool      `json:"ok"`
	Set     *corpusSet `json:"set"`
	Delete  string     `json:"delete"`
	Replace []string   `json:"replace"`
}

type corpusSet struct {
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

var corpusPathPart = regexp.MustCompile(`[^.\[\]]+|\[\d+\]`)

// apply makes the case's document from the example's text.
func (c corpusCase) apply(example []byte) ([]byte, error) {
	switch {
	case c.Replace != nil:
		if len(c.Replace) != 2 || bytes.Count(example, []byte(c.Replace[0])) != 1 {
			return nil, fmt.Errorf("replace needs [old, new] with old once in the example")
		}
		return bytes.Replace(example, []byte(c.Replace[0]), []byte(c.Replace[1]), 1), nil
	case c.Set != nil || c.Delete != "":
		dec := json.NewDecoder(bytes.NewReader(example))
		dec.UseNumber()
		var doc any
		if err := dec.Decode(&doc); err != nil {
			return nil, err
		}
		path := c.Delete
		if c.Set != nil {
			path = c.Set.Path
		}
		parts := corpusPathPart.FindAllString(path, -1)
		var parent any = doc
		for _, p := range parts[:len(parts)-1] {
			parent = corpusStep(parent, p)
			if parent == nil {
				return nil, fmt.Errorf("no %s in the example", path)
			}
		}
		last := parts[len(parts)-1]
		var value any
		if c.Set != nil {
			d := json.NewDecoder(bytes.NewReader(c.Set.Value))
			d.UseNumber()
			if err := d.Decode(&value); err != nil {
				return nil, err
			}
		}
		switch p := parent.(type) {
		case map[string]any:
			if c.Set != nil {
				p[last] = value
			} else {
				delete(p, last)
			}
		case []any:
			i, err := strconv.Atoi(strings.Trim(last, "[]"))
			if err != nil || i >= len(p) {
				return nil, fmt.Errorf("no %s in the example", path)
			}
			if c.Set != nil {
				p[i] = value
			} else {
				return nil, fmt.Errorf("delete a list element by setting its list")
			}
		}
		return json.Marshal(doc)
	}
	return nil, fmt.Errorf("a case needs set, delete or replace")
}

func corpusStep(v any, part string) any {
	if strings.HasPrefix(part, "[") {
		l, _ := v.([]any)
		i, err := strconv.Atoi(strings.Trim(part, "[]"))
		if err != nil || i >= len(l) {
			return nil
		}
		return l[i]
	}
	m, _ := v.(map[string]any)
	return m[part]
}

func TestConfigExplorerCorpusIsLoadsVerdict(t *testing.T) {
	example, err := os.ReadFile("../../testdata/behaviour.example.json")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.ReadFile("../../testdata/configtool/cases.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	var ok, refused int
	for n, line := range bytes.Split(bytes.TrimSpace(f), []byte("\n")) {
		var c corpusCase
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil || c.ID == "" || c.OK == nil {
			t.Fatalf("cases.jsonl line %d: want {id, ok, set|delete|replace}: %v", n+1, err)
		}
		if ids[c.ID] {
			t.Fatalf("cases.jsonl line %d: id %q twice", n+1, c.ID)
		}
		ids[c.ID] = true
		doc, err := c.apply(example)
		if err != nil {
			t.Fatalf("cases.jsonl line %d (%s): %v", n+1, c.ID, err)
		}
		_, _, lerr := Load(bytes.NewReader(doc))
		if got := lerr == nil; got != *c.OK {
			t.Errorf("%s: cases.jsonl says ok=%v, Load says %v (%v)", c.ID, *c.OK, got, lerr)
		}
		if *c.OK {
			ok++
		} else {
			refused++
		}
	}
	// Both verdicts must be well represented, or the corpus proves little.
	if ok < 100 || refused < 100 {
		t.Errorf("cases.jsonl has %d accepted and %d refused; want 100 or more of each", ok, refused)
	}
}
