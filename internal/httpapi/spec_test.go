package httpapi

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// spec is the served OpenAPI document, decoded.
func specDoc(t *testing.T) map[string]any {
	t.Helper()
	w := get(newTestHandler(t, NewRecorder(1)), "/openapi.json", false)
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// Every route is in the spec, and every path in the spec is a route.
func TestSpecCoversTheRoutes(t *testing.T) {
	var inSpec []string
	for p := range specDoc(t)["paths"].(map[string]any) {
		inSpec = append(inSpec, p)
	}
	sort.Strings(inSpec)
	routed := slices.Clone(Routes)
	sort.Strings(routed)
	if !slices.Equal(inSpec, routed) {
		t.Errorf("spec paths %v\nroutes     %v", inSpec, routed)
	}
}

// validator checks JSON values against the spec's schemas, noting each
// property it sees, so a test can ask which declared ones never appeared.
type validator struct {
	doc  map[string]any
	seen map[string]bool
	errs []string
}

func (v *validator) resolve(s map[string]any, at string) (map[string]any, string) {
	for {
		ref, ok := s["$ref"].(string)
		if !ok {
			return s, at
		}
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		s, at = v.doc["components"].(map[string]any)["schemas"].(map[string]any)[name].(map[string]any), ref
	}
}

func jsonType(x any) string {
	switch x := x.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if x == math.Trunc(x) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", x)
}

func (v *validator) fail(path, format string, a ...any) {
	v.errs = append(v.errs, path+": "+fmt.Sprintf(format, a...))
}

func (v *validator) check(path string, x any, s map[string]any, at string) {
	s, at = v.resolve(s, at)
	if alts, ok := s["oneOf"].([]any); ok {
		var pass []*validator
		for i, a := range alts {
			sub := &validator{doc: v.doc, seen: map[string]bool{}}
			sub.check(path, x, a.(map[string]any), at+"/oneOf/"+strconv.Itoa(i))
			if len(sub.errs) == 0 {
				pass = append(pass, sub)
			}
		}
		if len(pass) != 1 {
			v.fail(path, "matches %d of oneOf", len(pass))
			return
		}
		for k := range pass[0].seen {
			v.seen[k] = true
		}
		return
	}
	if ty, ok := s["type"]; ok {
		var types []string
		switch ty := ty.(type) {
		case string:
			types = []string{ty}
		case []any:
			for _, t := range ty {
				types = append(types, t.(string))
			}
		}
		got := jsonType(x)
		if !slices.Contains(types, got) && !(got == "integer" && slices.Contains(types, "number")) {
			v.fail(path, "%s, want %v", got, types)
			return
		}
	}
	if enum, ok := s["enum"].([]any); ok && !slices.Contains(enum, x) {
		v.fail(path, "%v isn't one of %v", x, enum)
	}
	if str, ok := x.(string); ok && s["format"] == "date-time" {
		if _, err := time.Parse(time.RFC3339, str); err != nil {
			v.fail(path, "%q isn't a date-time", str)
		}
	}
	switch x := x.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		for _, r := range anys(s["required"]) {
			if _, ok := x[r.(string)]; !ok {
				v.fail(path, "no %s", r)
			}
		}
		if props == nil {
			return // an open object (the behaviour document)
		}
		for k, val := range x {
			ps, ok := props[k].(map[string]any)
			if !ok {
				if s["additionalProperties"] == false {
					v.fail(path, "%s isn't in the spec", k)
				}
				continue
			}
			key := at + "/properties/" + k
			if val != nil { // a property only ever null leaves its other branch unchecked
				v.seen[key] = true
			}
			v.check(path+"."+k, val, ps, key)
		}
	case []any:
		if items, ok := s["items"].(map[string]any); ok {
			for i, val := range x {
				v.check(path+"["+strconv.Itoa(i)+"]", val, items, at+"/items")
			}
		}
	}
}

// declared is every property the spec declares, by where check notes it.
func (v *validator) declared() []string {
	var out []string
	var walk func(s map[string]any, at string)
	walk = func(s map[string]any, at string) {
		if _, ok := s["$ref"]; ok {
			return
		}
		for i, a := range anys(s["oneOf"]) {
			walk(a.(map[string]any), at+"/oneOf/"+strconv.Itoa(i))
		}
		if items, ok := s["items"].(map[string]any); ok {
			walk(items, at+"/items")
		}
		props, _ := s["properties"].(map[string]any)
		for k, p := range props {
			out = append(out, at+"/properties/"+k)
			walk(p.(map[string]any), at+"/properties/"+k)
		}
	}
	for name, s := range v.doc["components"].(map[string]any)["schemas"].(map[string]any) {
		walk(s.(map[string]any), "#/components/schemas/"+name)
	}
	sort.Strings(out)
	return out
}

func anys(x any) []any { a, _ := x.([]any); return a }

// responseSchema is the spec's schema for route's GET answering code.
func (v *validator) responseSchema(t *testing.T, route string, code int) map[string]any {
	t.Helper()
	r := v.doc["paths"].(map[string]any)[route].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)[strconv.Itoa(code)]
	if r == nil {
		t.Fatalf("%s has no %d response", route, code)
	}
	resp, _ := v.resolveResponse(r.(map[string]any))
	return resp["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
}

func (v *validator) resolveResponse(r map[string]any) (map[string]any, string) {
	if ref, ok := r["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/components/responses/")
		return v.doc["components"].(map[string]any)["responses"].(map[string]any)[name].(map[string]any), ref
	}
	return r, ""
}

func (v *validator) body(t *testing.T, route string, code int, b []byte) {
	t.Helper()
	var x any
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatalf("%s: %v: %s", route, err, b)
	}
	v.check(route, x, v.responseSchema(t, route, code), route)
}

// Every response the API gives over every fixture fits the spec, and every
// property the spec declares appears in one of them: the spec says what
// the API sends, no more and no less.
func TestSpecMatchesEveryResponse(t *testing.T) {
	v := &validator{doc: specDoc(t), seen: map[string]bool{}}
	for _, path := range fixtures(t) {
		rec := NewRecorder(50)
		_, _, raw := exampleBehaviour(t)
		rec.Behaviour(raw, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), 50)
		n := 0
		replayInto(t, path, rec, func(s *Snapshot) {
			if n++; n%7 != 0 {
				return
			}
			v.body(t, "/api/v1/state", 200, s.State)
			for _, id := range s.TankIDs {
				v.body(t, "/api/v1/tanks/{id}", 200, s.Tanks[id])
			}
		})
		h := newTestHandler(t, rec)
		for route, p := range map[string]string{"/api/v1/state": "/api/v1/state", "/api/v1/draws": "/api/v1/draws",
			"/api/v1/model": "/api/v1/model", "/api/v1/tanks/{id}": "/api/v1/tanks/hot_water_cylinder_left"} {
			w := get(h, p, true)
			if w.Code != http.StatusOK {
				t.Fatalf("%s: %d", p, w.Code)
			}
			v.body(t, route, 200, w.Body.Bytes())
		}
		if len(v.errs) > 0 {
			t.Fatalf("%s:\n%s", path, strings.Join(v.errs[:min(len(v.errs), 20)], "\n"))
		}
	}
	h := newTestHandler(t, showerRecorder(t))
	v.body(t, "/api/v1/tanks/{id}", 404, get(h, "/api/v1/tanks/nope", true).Body.Bytes())
	v.body(t, "/api/v1/state", 503, get(newTestHandler(t, NewRecorder(1)), "/api/v1/state", true).Body.Bytes())
	if len(v.errs) > 0 {
		t.Fatal(strings.Join(v.errs, "\n"))
	}
	var never []string
	for _, k := range v.declared() {
		if !v.seen[k] {
			never = append(never, k)
		}
	}
	if len(never) > 0 {
		t.Errorf("declared but never sent:\n%s", strings.Join(never, "\n"))
	}
}

// Every 503 the spec declares requires Retry-After, and the draws and the
// model declare the auth 503 too (any /api/ route can meet it).
func TestSpec503sRequireRetryAfter(t *testing.T) {
	v := &validator{doc: specDoc(t), seen: map[string]bool{}}
	for _, route := range []string{"/api/v1/state", "/api/v1/tanks/{id}", "/api/v1/draws", "/api/v1/model"} {
		r, _ := v.doc["paths"].(map[string]any)[route].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)["503"].(map[string]any)
		if r == nil {
			t.Errorf("%s has no 503", route)
			continue
		}
		resp, _ := v.resolveResponse(r)
		h, _ := resp["headers"].(map[string]any)["Retry-After"].(map[string]any)
		if h == nil || h["required"] != true {
			t.Errorf("%s: 503 Retry-After %v", route, h)
		}
	}
}
