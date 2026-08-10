package config

import (
	"testing"
	"time"
)

func TestServeIdleShutdownResolution(t *testing.T) {
	if got := (ServeConfig{}).IdleShutdown(); got != DefaultServeIdleShutdown {
		t.Fatalf("omitted field = %v, want default %v", got, DefaultServeIdleShutdown)
	}
	zero := 0
	if got := (ServeConfig{IdleShutdownSeconds: &zero}).IdleShutdown(); got != 0 {
		t.Fatalf("explicit 0 = %v, want disabled", got)
	}
	neg := -1
	if got := (ServeConfig{IdleShutdownSeconds: &neg}).IdleShutdown(); got != DefaultServeIdleShutdown {
		t.Fatalf("negative = %v, want default %v", got, DefaultServeIdleShutdown)
	}
	five := 300
	if got := (ServeConfig{IdleShutdownSeconds: &five}).IdleShutdown(); got != 5*time.Minute {
		t.Fatalf("300s = %v, want 5m", got)
	}
}
