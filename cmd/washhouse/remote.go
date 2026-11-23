package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sweeney/common/remoteconfig"
	"github.com/sweeney/identity/common/auth"
	"github.com/sweeney/washhouse/internal/behaviour"
	"github.com/sweeney/washhouse/internal/engine"
)

// document is a behaviour document that behaviour.Load accepted.
type document struct {
	raw []byte // compacted: a reformatted document is the same document
	bc  behaviour.Config
	ec  engine.Config
}

func parseDocument(b []byte) (document, error) {
	bc, ec, err := behaviour.Load(bytes.NewReader(b))
	if err != nil {
		return document{}, err
	}
	var c bytes.Buffer
	if err := json.Compact(&c, b); err != nil {
		return document{}, err
	}
	return document{raw: c.Bytes(), bc: bc, ec: ec}, nil
}

// getter is what washhouse needs from the config service:
// remoteconfig.Client.
type getter interface {
	Get(ctx context.Context, ns string, dst any) error
}

// remote is the behaviour namespace: each fetch, validated, and its
// outcome for /healthz. Keeping the last good document is the service's
// (service.apply only ever takes a document that parsed).
type remote struct {
	get     getter
	ns      string
	base    string // for the log
	tracker remoteconfig.Tracker
}

// newRemote reads the client secret and builds the client.
func newRemote(lc localConfig, version string) (*remote, error) {
	b, err := os.ReadFile(lc.Identity.ClientSecretFile)
	if err != nil {
		return nil, fmt.Errorf("identity client secret: %w", err)
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return nil, fmt.Errorf("identity client secret: %s is empty", lc.Identity.ClientSecretFile)
	}
	return &remote{
		get: &remoteconfig.Client{
			BaseURL:   lc.RemoteConfig.BaseURL,
			Tokens:    &auth.TokenSource{BaseURL: lc.Identity.BaseURL, ClientID: lc.Identity.ClientID, ClientSecret: secret},
			UserAgent: "washhouse/" + version,
		},
		ns:   lc.RemoteConfig.Namespace,
		base: lc.RemoteConfig.BaseURL,
	}, nil
}

// fetch gets and parses the namespace, recording the outcome: a document
// that fetched but was refused never counts as landed.
func (r *remote) fetch(ctx context.Context) (document, error) {
	var raw json.RawMessage
	err := r.get.Get(ctx, r.ns, &raw)
	var d document
	if err == nil {
		d, err = parseDocument(raw)
	}
	if err != nil {
		err = fmt.Errorf("%s: %w", r.ns, err)
	}
	r.tracker.Record(r.ns, err)
	return d, err
}

// refused records that the service refused a document that parsed.
func (r *remote) refused(err error) { r.tracker.Record(r.ns, err) }

// status is the namespace's latest outcome (zero before any).
func (r *remote) status() remoteconfig.Status { return r.tracker.Statuses()[r.ns] }

var errNoLoop = errors.New("washhouse is stopping")
