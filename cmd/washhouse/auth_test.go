package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeIdentity serves a JWKS for one ES256 key and signs tokens with it,
// as identity does; down makes its JWKS fail.
type fakeIdentity struct {
	*httptest.Server
	key  *ecdsa.PrivateKey
	down atomic.Bool
}

func newFakeIdentity(t *testing.T) *fakeIdentity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeIdentity{key: key}
	fi.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" || fi.down.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		b := func(x []byte) string { return base64.RawURLEncoding.EncodeToString(x) }
		pub, _ := key.PublicKey.Bytes() // 0x04 || X || Y
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "use": "sig", "alg": "ES256", "kid": "k1", "crv": "P-256", "x": b(pub[1:33]), "y": b(pub[33:]),
		}}})
	}))
	t.Cleanup(fi.Close)
	return fi
}

func (fi *fakeIdentity) sign(t *testing.T, typ string, claims jwt.MapClaims) string {
	t.Helper()
	claims["iss"] = fi.URL
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = "k1"
	if typ != "" {
		tok.Header["typ"] = typ
	}
	s, err := tok.SignedString(fi.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The API's middleware is identity's verifier: an active user's token or
// a service's, the audience checked as http.audience says, and a 503
// (never a 401) when identity's keys can't be had.
func TestAuthWithIdentitysTokens(t *testing.T) {
	fi := newFakeIdentity(t)
	user := func(aud string, active bool) jwt.MapClaims {
		return jwt.MapClaims{"sub": "u1", "usr": "someone", "rol": "user", "act": active, "aud": aud}
	}
	tokens := map[string]string{
		"user":           fi.sign(t, "", user("washhouse", true)),
		"user elsewhere": fi.sign(t, "", user("greenhouse", true)),
		"inactive user":  fi.sign(t, "", user("washhouse", false)),
		"service":        fi.sign(t, "at+jwt", jwt.MapClaims{"client_id": "statehouse", "aud": "washhouse"}),
		"expired":        fi.sign(t, "", jwt.MapClaims{"sub": "u1", "act": true, "aud": "washhouse", "exp": time.Now().Add(-time.Hour).Unix()}),
	}
	if w := httptestDo(t, fi, "warn", tokens["expired"]); w.Header().Get("WWW-Authenticate") !=
		`Bearer realm="`+fi.URL+`", error="invalid_token", error_description="token expired"` {
		t.Errorf("expired: %q", w.Header().Get("WWW-Authenticate"))
	}
	for _, tc := range []struct {
		token string
		want  map[string]int // by mode
	}{
		{"user", map[string]int{"off": 200, "warn": 200, "enforce": 200}},
		{"user elsewhere", map[string]int{"off": 200, "warn": 200, "enforce": 401}},
		{"inactive user", map[string]int{"off": 401, "warn": 401, "enforce": 401}},
		{"service", map[string]int{"off": 200, "warn": 200, "enforce": 200}},
		{"expired", map[string]int{"off": 401, "warn": 401, "enforce": 401}},
	} {
		for _, mode := range []string{"off", "warn", "enforce"} {
			lc := localConfig{Identity: identityConfig{BaseURL: fi.URL}, HTTP: httpConfig{Audience: mode}}
			mw, err := newAuth(lc, nil)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
			r.Header.Set("Authorization", "Bearer "+tokens[tc.token])
			mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(w, r)
			if w.Code != tc.want[mode] {
				t.Errorf("%s, audience %s: %d, want %d", tc.token, mode, w.Code, tc.want[mode])
			}
			if w.Code == 401 && !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), `Bearer realm="`+fi.URL+`", error="invalid_token"`) {
				t.Errorf("challenge %q", w.Header().Get("WWW-Authenticate"))
			}
		}
	}

	// warn accepts a token for another audience, and logs it: each line is
	// a caller enforcing would lock out. off logs nothing.
	for mode, logs := range map[string]bool{"warn": true, "off": false} {
		var buf bytes.Buffer
		mw, err := newAuth(localConfig{Identity: identityConfig{BaseURL: fi.URL}, HTTP: httpConfig{Audience: mode}},
			slog.New(slog.NewTextHandler(&buf, nil)))
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
		r.Header.Set("Authorization", "Bearer "+tokens["user elsewhere"])
		mw(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), r)
		if got := strings.Contains(buf.String(), "washhouse"); got != logs {
			t.Errorf("%s logged %q", mode, buf.String())
		}
	}

	// identity down, with no keys yet: the token can't be checked.
	fi.down.Store(true)
	mw, err := newAuth(localConfig{Identity: identityConfig{BaseURL: fi.URL}, HTTP: httpConfig{Audience: "warn"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	r.Header.Set("Authorization", "Bearer "+tokens["user"])
	mw(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Errorf("identity down: %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}

	if _, err := newAuth(localConfig{Identity: identityConfig{BaseURL: fi.URL}, HTTP: httpConfig{Audience: "strict"}}, nil); err == nil {
		t.Error("an unknown audience mode was taken")
	}
}

func httptestDo(t *testing.T, fi *fakeIdentity, mode, token string) *httptest.ResponseRecorder {
	t.Helper()
	mw, err := newAuth(localConfig{Identity: identityConfig{BaseURL: fi.URL}, HTTP: httpConfig{Audience: mode}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	mw(http.NotFoundHandler()).ServeHTTP(w, r)
	return w
}

// identity/common v0.7.0 reports an unknown kid as keys unavailable while
// its refetch is throttled, so a forged token (or one whose key was
// rotated out) is a 401 the first time and a 503 for about 10 s after
// (tanktemp's review of sweeney/common#17). This pins that behaviour: when
// identity fixes it, this test fails, and apiauth's doc and this test
// change with the bump. A real token is unaffected.
func TestIdentityThrottledKidMissIsA503(t *testing.T) {
	fi := newFakeIdentity(t)
	real := fi.sign(t, "", jwt.MapClaims{"sub": "u1", "act": true, "aud": "washhouse"})
	forged := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"sub": "x", "iss": fi.URL, "exp": time.Now().Add(time.Hour).Unix()})
		tok.Header["kid"] = "made-up"
		s, err := tok.SignedString(fi.key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}()
	mw, err := newAuth(localConfig{Identity: identityConfig{BaseURL: fi.URL}, HTTP: httpConfig{Audience: "off"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(tok string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(w, r)
		return w.Code
	}
	if got := []int{do(real), do(forged), do(forged), do(real)}; got[0] != 200 || got[1] != 401 || got[2] != 503 || got[3] != 200 {
		t.Errorf("real, forged, forged, real: %v; want [200 401 503 200] (identity v0.7.0's throttled kid miss)", got)
	}
}
