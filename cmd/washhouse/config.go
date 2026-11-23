package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sweeney/common/remoteconfig"

	"github.com/sweeney/washhouse/internal/ingest"
	"github.com/sweeney/washhouse/internal/publish"
)

// localConfig is the local YAML file (deploy/config.example.yaml): the
// broker, HTTP, where things are on the broker, and where the behaviour
// document is. Unknown keys are refused (yaml.v3 also refuses a
// duplicate), every key is required but the three optional MQTT ones, and
// the topics and URLs are checked.
type localConfig struct {
	MQTT         mqttConfig     `yaml:"mqtt"`
	HTTP         httpConfig     `yaml:"http"`
	RemoteConfig remoteConfig   `yaml:"remote_config"`
	Identity     identityConfig `yaml:"identity"`
	Site         siteConfig     `yaml:"site"`
	Influx       influxConfig   `yaml:"influx"` // optional: none, no history
	Heartbeat    duration       `yaml:"heartbeat"`
}

// remoteConfig is the config service and the behaviour namespace on it.
type remoteConfig struct {
	BaseURL   string `yaml:"base_url"`
	Namespace string `yaml:"namespace"`
}

// identityConfig is the client washhouse reads the namespace as.
type identityConfig struct {
	BaseURL          string `yaml:"base_url"`
	ClientID         string `yaml:"client_id"`
	ClientSecretFile string `yaml:"client_secret_file"`
}

type mqttConfig struct {
	Broker        string `yaml:"broker"`
	ClientID      string `yaml:"client_id"`
	Username      string `yaml:"username"`
	PasswordFile  string `yaml:"password_file"`
	BaseTopic     string `yaml:"base_topic"`
	TanktempTopic string `yaml:"tanktemp_topic"`
	BoilerTopic   string `yaml:"boiler_topic"`
}

type httpConfig struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"public_url"` // optional: the spec's server
	// Audience is whether a token must name washhouse: off, warn or enforce.
	Audience string `yaml:"audience"`
}

// The audience modes (http.audience).
const (
	audienceOff     = "off"
	audienceWarn    = "warn"
	audienceEnforce = "enforce"
)

// duration is a time.Duration written as a Go duration string ("15m").
type duration time.Duration

func (d *duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a duration must be a string such as 15m, got %q", n.Line, n.Value)
	}
	x, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = duration(x)
	return nil
}

func loadLocal(b []byte) (localConfig, error) {
	var c localConfig
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return localConfig{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return localConfig{}, errors.New("the config must be one document")
	}
	for _, r := range []struct{ key, v string }{
		{"mqtt.broker", c.MQTT.Broker}, {"mqtt.base_topic", c.MQTT.BaseTopic},
		{"mqtt.tanktemp_topic", c.MQTT.TanktempTopic}, {"mqtt.boiler_topic", c.MQTT.BoilerTopic},
		{"http.listen", c.HTTP.Listen}, {"http.audience", c.HTTP.Audience},
		{"remote_config.base_url", c.RemoteConfig.BaseURL}, {"remote_config.namespace", c.RemoteConfig.Namespace},
		{"identity.base_url", c.Identity.BaseURL}, {"identity.client_id", c.Identity.ClientID},
		{"identity.client_secret_file", c.Identity.ClientSecretFile},
		{"site.id", c.Site.ID},
	} {
		if r.v == "" {
			return localConfig{}, fmt.Errorf("%s is required", r.key)
		}
	}
	if c.Heartbeat <= 0 {
		return localConfig{}, errors.New("heartbeat is required, and more than 0")
	}
	for _, r := range []struct{ key, v string }{
		{"remote_config.base_url", c.RemoteConfig.BaseURL}, {"identity.base_url", c.Identity.BaseURL},
	} {
		if err := secureURL(r.v); err != nil {
			return localConfig{}, fmt.Errorf("%s: %w", r.key, err)
		}
	}
	if !siteID.MatchString(c.Site.ID) {
		return localConfig{}, fmt.Errorf("site.id %q must match %s", c.Site.ID, siteID)
	}
	if c.Influx != (influxConfig{}) {
		for _, r := range []struct{ key, v string }{
			{"influx.url", c.Influx.URL}, {"influx.org", c.Influx.Org},
			{"influx.bucket", c.Influx.Bucket}, {"influx.token_file", c.Influx.TokenFile},
		} {
			if r.v == "" {
				return localConfig{}, fmt.Errorf("%s is required when influx is set", r.key)
			}
		}
		if err := secureURL(c.Influx.URL); err != nil {
			return localConfig{}, fmt.Errorf("influx.url: %w", err)
		}
	}
	switch c.HTTP.Audience {
	case audienceOff, audienceWarn, audienceEnforce:
	default:
		return localConfig{}, fmt.Errorf("http.audience is %q: off, warn or enforce", c.HTTP.Audience)
	}
	if c.HTTP.PublicURL != "" {
		if u, err := url.Parse(c.HTTP.PublicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return localConfig{}, fmt.Errorf("http.public_url %q must be an http or https URL", c.HTTP.PublicURL)
		}
	}
	if err := remoteconfig.ValidName(c.RemoteConfig.Namespace); err != nil {
		return localConfig{}, fmt.Errorf("remote_config.namespace: %w", err)
	}
	if _, err := publish.New(c.MQTT.BaseTopic); err != nil {
		return localConfig{}, err
	}
	if err := c.sources("").Validate(); err != nil {
		return localConfig{}, err
	}
	return c, nil
}

// siteID is a site's id, as in the config service's sites namespace.
var siteID = regexp.MustCompile(`^[a-z0-9_-]+$`)

// secureURL refuses a URL a bearer token or a client secret would cross
// in cleartext: https, or http to this host (for tests).
func secureURL(s string) error {
	u, err := url.Parse(s)
	if err == nil && u.Host != "" {
		switch {
		case u.Scheme == "https":
			return nil
		case u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()):
			return nil
		}
	}
	return fmt.Errorf("%q must be an https URL (or http to this host)", s)
}

// sources are the topics washhouse reads; ambient comes from the
// behaviour document.
func (c localConfig) sources(ambient string) ingest.Sources {
	return ingest.Sources{Tanktemp: []string{c.MQTT.TanktempTopic}, Boiler: c.MQTT.BoilerTopic, Ambient: ambient}
}

// influxConfig is where the history goes; the section is optional.
type influxConfig struct {
	URL       string `yaml:"url"`
	Org       string `yaml:"org"`
	Bucket    string `yaml:"bucket"`
	TokenFile string `yaml:"token_file"`
}

// siteConfig is the property washhouse reports for.
type siteConfig struct {
	ID string `yaml:"id"`
}
