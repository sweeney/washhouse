package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// configService is identity and the config service in one httptest server:
// a client-credentials token, and the namespace's document, which a test
// can change.
type configService struct {
	*httptest.Server
	mu        sync.Mutex
	doc       string
	code      int
	agents    []string
	posted    []string // client_id:client_secret per token request
	bearers   []string
	namespace string
}

func newConfigService(t *testing.T) *configService {
	t.Helper()
	b, err := os.ReadFile(behaviourExample)
	if err != nil {
		t.Fatal(err)
	}
	cs := &configService{doc: string(b), code: http.StatusOK, namespace: "washhouse"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		cs.mu.Lock()
		cs.posted = append(cs.posted, r.PostForm.Get("client_id")+":"+r.PostForm.Get("client_secret"))
		cs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-1","expires_in":3600,"token_type":"Bearer"}`))
	})
	mux.HandleFunc("GET /api/v1/config/{ns}", func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		defer cs.mu.Unlock()
		cs.agents = append(cs.agents, r.UserAgent())
		cs.bearers = append(cs.bearers, r.Header.Get("Authorization"))
		if r.PathValue("ns") != cs.namespace {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if cs.code != http.StatusOK {
			http.Error(w, `{"error":"no"}`, cs.code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cs.doc))
	})
	cs.Server = httptest.NewServer(mux)
	t.Cleanup(cs.Close)
	return cs
}

func (cs *configService) set(doc string, code int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.doc, cs.code = doc, code
}

// localFor is the example local config pointed at cs, with a secret file.
func localFor(t *testing.T, cs *configService) localConfig {
	t.Helper()
	lc, err := loadLocal([]byte(readLocalExample(t)))
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lc.RemoteConfig.BaseURL, lc.Identity.BaseURL, lc.Identity.ClientSecretFile = cs.URL, cs.URL, secret
	return lc
}

func TestFetchReadsTheNamespace(t *testing.T) {
	cs := newConfigService(t)
	r, err := newRemote(localFor(t, cs), "abc1234")
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ec.Tanks) != 2 || d.bc.AmbientTopic == "" || len(d.raw) == 0 {
		t.Errorf("document %+v", d.bc)
	}
	if strings.ContainsAny(string(d.raw), "\n ") {
		t.Errorf("raw isn't compact: %.60s", d.raw)
	}
	if want := []string{"washhouse:s3cret"}; strings.Join(cs.posted, ",") != strings.Join(want, ",") {
		t.Errorf("token requests %v, want %v (the secret file's line)", cs.posted, want)
	}
	if cs.bearers[0] != "Bearer tok-1" || cs.agents[0] != "washhouse/abc1234" {
		t.Errorf("bearer %q, agent %q", cs.bearers[0], cs.agents[0])
	}
	if st := r.status(); !st.OK || st.FetchedAt.IsZero() {
		t.Errorf("status %+v", st)
	}
}

// A document washhouse can't use fails as the fetch would, and is
// recorded as a failure: it never counts as landed.
func TestFetchRefusesABadDocument(t *testing.T) {
	cs := newConfigService(t)
	cs.set(`{"colour":"blue"}`, http.StatusOK)
	r, err := newRemote(localFor(t, cs), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("got %v", err)
	}
	if st := r.status(); st.OK || !strings.Contains(st.Error, "colour") || !strings.Contains(st.Error, "washhouse") {
		t.Errorf("status %+v", st)
	}
}

func TestFetchFailsWhenTheNamespaceIsMissing(t *testing.T) {
	cs := newConfigService(t)
	lc := localFor(t, cs)
	lc.RemoteConfig.Namespace = "elsewhere"
	r, err := newRemote(lc, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v", err)
	}
}

func TestRemoteNeedsTheSecretFile(t *testing.T) {
	cs := newConfigService(t)
	lc := localFor(t, cs)
	lc.Identity.ClientSecretFile = filepath.Join(t.TempDir(), "nope")
	if _, err := newRemote(lc, "dev"); err == nil || !strings.Contains(err.Error(), "client secret") {
		t.Errorf("got %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lc.Identity.ClientSecretFile = empty
	if _, err := newRemote(lc, "dev"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("got %v", err)
	}
}

func TestParseDocumentRefusesWhatLoadRefuses(t *testing.T) {
	if _, err := parseDocument([]byte(`{"colour":"blue"}`)); err == nil {
		t.Error("no error")
	}
}
