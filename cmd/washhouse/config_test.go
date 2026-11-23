package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

const exampleLocal = "../../deploy/config.example.yaml"

func readLocalExample(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(exampleLocal)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExampleLocalConfigLoads(t *testing.T) {
	c, err := loadLocal([]byte(readLocalExample(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := localConfig{
		MQTT: mqttConfig{Broker: "tcp://broker.example:1883", ClientID: "washhouse", Username: "washhouse",
			PasswordFile: "/etc/washhouse/mqtt-password", BaseTopic: "water/hot",
			TanktempTopic: "water/hot/tanktemp", BoilerTopic: "energy/boiler/sensor"},
		HTTP:         httpConfig{Listen: "127.0.0.1:8095", PublicURL: "https://washhouse.example", Audience: "warn"},
		RemoteConfig: remoteConfig{BaseURL: "https://config.example", Namespace: "washhouse"},
		Identity: identityConfig{BaseURL: "https://id.example", ClientID: "washhouse",
			ClientSecretFile: "/etc/washhouse/client-secret"},
		Heartbeat: duration(15 * time.Minute),
	}
	if c != want {
		t.Errorf("got  %+v\nwant %+v", c, want)
	}
}

func TestLocalConfigIsStrict(t *testing.T) {
	ex := readLocalExample(t)
	for _, tc := range []struct {
		name, from, to, want string
	}{
		{"unknown key", "heartbeat: 15m", "heartbeat: 15m\ncolour: blue", "colour"},
		{"unknown nested key", "  broker: tcp://broker.example:1883", "  broker: tcp://broker.example:1883\n  port: 1883", "port"},
		{"case-variant key", "heartbeat: 15m", "Heartbeat: 15m", "Heartbeat"},
		{"duplicate key", "heartbeat: 15m", "heartbeat: 15m\nheartbeat: 5m", "heartbeat"},
		{"no broker", "  broker: tcp://broker.example:1883\n", "", "mqtt.broker"},
		{"no base topic", "  base_topic: water/hot\n", "", "mqtt.base_topic"},
		{"no tanktemp topic", "  tanktemp_topic: water/hot/tanktemp\n", "", "mqtt.tanktemp_topic"},
		{"no boiler topic", "  boiler_topic: energy/boiler/sensor\n", "", "mqtt.boiler_topic"},
		{"no listen", "  listen: 127.0.0.1:8095\n", "", "http.listen"},
		{"no audience", "  audience: warn\n", "", "http.audience"},
		{"a bad audience", "  audience: warn", "  audience: strict", "http.audience"},
		{"a public URL that isn't one", "public_url: https://washhouse.example", "public_url: washhouse.example", "http.public_url"},
		{"a behaviour file", "heartbeat: 15m", "heartbeat: 15m\nbehaviour_file: /etc/washhouse/behaviour.json", "behaviour_file"},
		{"no remote base", "  base_url: https://config.example\n", "", "remote_config.base_url"},
		{"no namespace", "  namespace: washhouse\n", "", "remote_config.namespace"},
		{"a bad namespace", "namespace: washhouse", "namespace: Wash House", "namespace"},
		{"no identity base", "  base_url: https://id.example\n", "", "identity.base_url"},
		{"no client id", "  client_id: washhouse\n", "", "identity.client_id"},
		{"no client secret", "  client_secret_file: /etc/washhouse/client-secret   # one line\n", "", "identity.client_secret_file"},
		{"a secret in the file", "  client_secret_file: /etc/washhouse/client-secret", "  client_secret: hunter2", "client_secret"},
		{"config in cleartext", "base_url: https://config.example", "base_url: http://config.example", "https"},
		{"identity in cleartext", "base_url: https://id.example", "base_url: http://id.example", "https"},
		{"a URL that isn't one", "base_url: https://config.example", "base_url: config.example", "https"},
		{"a URL with no host", "base_url: https://config.example", "base_url: https:///config", "https"},
		{"no heartbeat", "heartbeat: 15m\n", "", "heartbeat"},
		{"heartbeat as a number", "heartbeat: 15m", "heartbeat: 900", "duration"},
		{"heartbeat 0", "heartbeat: 15m", "heartbeat: 0s", "heartbeat"},
		{"wildcard base", "base_topic: water/hot", "base_topic: water/+", "wildcard"},
		{"base inside tanktemp's", "base_topic: water/hot", "base_topic: water/hot/tanktemp/x", "tanktemp"},
		{"wildcard source", "tanktemp_topic: water/hot/tanktemp", "tanktemp_topic: water/hot/#", "wildcard"},
		{"two documents", "heartbeat: 15m", "heartbeat: 15m\n---\nheartbeat: 5m", "one document"},
		{"a password in the file", "  password_file: /etc/washhouse/mqtt-password", "  password: hunter2", "password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(ex, tc.from) {
				t.Fatalf("%q isn't in the example", tc.from)
			}
			_, err := loadLocal([]byte(strings.Replace(ex, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// Cleartext is allowed only to this host, for tests.
func TestLoopbackMayBeCleartext(t *testing.T) {
	ex := readLocalExample(t)
	for _, u := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		s := strings.Replace(ex, "https://config.example", u, 1)
		s = strings.Replace(s, "https://id.example", u, 1)
		if _, err := loadLocal([]byte(s)); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestOptionalLocalKeys(t *testing.T) {
	ex := readLocalExample(t)
	for _, drop := range []string{"  client_id: washhouse            # optional: washhouse-<hostname>\n",
		"  username: washhouse             # optional\n", "  public_url: https://washhouse.example   # optional: the API spec's server\n", "  password_file: /etc/washhouse/mqtt-password   # optional; one line\n"} {
		if !strings.Contains(ex, drop) {
			t.Fatalf("%q isn't in the example", drop)
		}
		if _, err := loadLocal([]byte(strings.Replace(ex, drop, "", 1))); err != nil {
			t.Errorf("without %q: %v", strings.TrimSpace(drop), err)
		}
	}
}
