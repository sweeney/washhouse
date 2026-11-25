package behaviour

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
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
