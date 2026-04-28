package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadFromFlags(t *testing.T) {
	t.Setenv("FREEPBX_EXPORTER_AMI_USERNAME", "")
	t.Setenv("FREEPBX_EXPORTER_AMI_SECRET", "")

	cfg, err := Load([]string{
		"-ami.address", "pbx.local:5038",
		"-ami.username", "promuser",
		"-ami.secret", "topsecret",
		"-ami.timeout", "5s",
		"-web.listen-address", ":9999",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AMIAddress != "pbx.local:5038" {
		t.Errorf("AMIAddress = %q", cfg.AMIAddress)
	}
	if cfg.AMITimeout != 5*time.Second {
		t.Errorf("AMITimeout = %v", cfg.AMITimeout)
	}
	if cfg.ListenAddress != ":9999" {
		t.Errorf("ListenAddress = %q", cfg.ListenAddress)
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("FREEPBX_EXPORTER_AMI_ADDRESS", "10.0.0.5:5038")
	t.Setenv("FREEPBX_EXPORTER_AMI_USERNAME", "envuser")
	t.Setenv("FREEPBX_EXPORTER_AMI_SECRET", "envsecret")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AMIUsername != "envuser" || cfg.AMISecret != "envsecret" {
		t.Errorf("env not applied: %+v", cfg.Redact())
	}
}

func TestFlagOverridesEnv(t *testing.T) {
	t.Setenv("FREEPBX_EXPORTER_AMI_USERNAME", "envuser")
	t.Setenv("FREEPBX_EXPORTER_AMI_SECRET", "envsecret")

	cfg, err := Load([]string{"-ami.username", "flaguser"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AMIUsername != "flaguser" {
		t.Errorf("flag did not override env: got %q", cfg.AMIUsername)
	}
	if cfg.AMISecret != "envsecret" {
		t.Errorf("env should still supply secret: got %q", cfg.AMISecret)
	}
}

func TestRedactHidesSecret(t *testing.T) {
	c := Config{AMISecret: "supersecret"}
	if got := c.Redact().AMISecret; got != "***" {
		t.Errorf("Redact = %q", got)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{
			name: "missing username",
			args: []string{"-ami.secret", "x"},
			want: "ami.username",
		},
		{
			name: "missing secret",
			args: []string{"-ami.username", "x"},
			want: "ami.secret",
		},
		{
			name: "bad metrics path",
			args: []string{"-ami.username", "u", "-ami.secret", "s", "-web.telemetry-path", "metrics"},
			want: "web.telemetry-path",
		},
		{
			name: "bad log level",
			args: []string{"-ami.username", "u", "-ami.secret", "s", "-log.level", "verbose"},
			want: "log.level",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FREEPBX_EXPORTER_AMI_USERNAME", "")
			t.Setenv("FREEPBX_EXPORTER_AMI_SECRET", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load(tc.args)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}
