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
		HTTP:          httpConfig{Listen: "127.0.0.1:8095"},
		BehaviourFile: "/etc/washhouse/behaviour.json",
		Heartbeat:     duration(15 * time.Minute),
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
		{"no behaviour file", "behaviour_file: /etc/washhouse/behaviour.json\n", "", "behaviour_file"},
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

func TestOptionalLocalKeys(t *testing.T) {
	ex := readLocalExample(t)
	for _, drop := range []string{"  client_id: washhouse\n", "  username: washhouse\n", "  password_file: /etc/washhouse/mqtt-password\n"} {
		if _, err := loadLocal([]byte(strings.Replace(ex, drop, "", 1))); err != nil {
			t.Errorf("without %q: %v", strings.TrimSpace(drop), err)
		}
	}
}
