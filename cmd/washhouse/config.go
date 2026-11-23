package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sweeney/washhouse/internal/ingest"
	"github.com/sweeney/washhouse/internal/publish"
)

// localConfig is the local YAML file (deploy/config.example.yaml): the
// broker, HTTP, and where things are on the broker. Unknown keys are
// refused (yaml.v3 also refuses a duplicate), every key is required but
// the three optional MQTT ones, and the topics are checked.
type localConfig struct {
	MQTT          mqttConfig `yaml:"mqtt"`
	HTTP          httpConfig `yaml:"http"`
	BehaviourFile string     `yaml:"behaviour_file"`
	Heartbeat     duration   `yaml:"heartbeat"`
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
	Listen string `yaml:"listen"`
}

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
		{"http.listen", c.HTTP.Listen}, {"behaviour_file", c.BehaviourFile},
	} {
		if r.v == "" {
			return localConfig{}, fmt.Errorf("%s is required", r.key)
		}
	}
	if c.Heartbeat <= 0 {
		return localConfig{}, errors.New("heartbeat is required, and more than 0")
	}
	if _, err := publish.New(c.MQTT.BaseTopic); err != nil {
		return localConfig{}, err
	}
	if err := c.sources("").Validate(); err != nil {
		return localConfig{}, err
	}
	return c, nil
}

// sources are the topics washhouse reads; ambient comes from the
// behaviour document.
func (c localConfig) sources(ambient string) ingest.Sources {
	return ingest.Sources{Tanktemp: []string{c.MQTT.TanktempTopic}, Boiler: c.MQTT.BoilerTopic, Ambient: ambient}
}
