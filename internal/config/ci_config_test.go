package config

import (
	"testing"
	"time"
)

func TestLoadCIDefaults(t *testing.T) {
	cfg, err := Load(fullEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CIMaxFixAttempts != 2 {
		t.Errorf("CIMaxFixAttempts = %d, want 2", cfg.CIMaxFixAttempts)
	}
	if cfg.CIFixBudget != 30*time.Minute {
		t.Errorf("CIFixBudget = %v, want 30m", cfg.CIFixBudget)
	}
	if cfg.CIPollInterval != 15*time.Second {
		t.Errorf("CIPollInterval = %v, want 15s", cfg.CIPollInterval)
	}
	if cfg.CIPollBudget != 12*time.Minute {
		t.Errorf("CIPollBudget = %v, want 12m", cfg.CIPollBudget)
	}
	if cfg.CIPollStall != 8*time.Minute {
		t.Errorf("CIPollStall = %v, want 8m", cfg.CIPollStall)
	}
	if cfg.CIPollMaxBudget != 25*time.Minute {
		t.Errorf("CIPollMaxBudget = %v, want 25m", cfg.CIPollMaxBudget)
	}
}

func TestLoadCIHonoursOverrides(t *testing.T) {
	cfg, err := Load(fullEnv(map[string]string{
		"CI_MAX_FIX_ATTEMPTS":   "3",
		"CI_FIX_BUDGET_MS":      "60000",
		"CI_POLL_INTERVAL_MS":   "5000",
		"CI_POLL_BUDGET_MS":     "120000",
		"CI_POLL_STALL_MS":      "90000",
		"CI_POLL_MAX_BUDGET_MS": "600000",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CIMaxFixAttempts != 3 {
		t.Errorf("CIMaxFixAttempts = %d, want 3", cfg.CIMaxFixAttempts)
	}
	if cfg.CIFixBudget != time.Minute {
		t.Errorf("CIFixBudget = %v, want 1m", cfg.CIFixBudget)
	}
	if cfg.CIPollInterval != 5*time.Second {
		t.Errorf("CIPollInterval = %v, want 5s", cfg.CIPollInterval)
	}
	if cfg.CIPollBudget != 2*time.Minute {
		t.Errorf("CIPollBudget = %v, want 2m", cfg.CIPollBudget)
	}
	if cfg.CIPollStall != 90*time.Second {
		t.Errorf("CIPollStall = %v, want 90s", cfg.CIPollStall)
	}
	if cfg.CIPollMaxBudget != 10*time.Minute {
		t.Errorf("CIPollMaxBudget = %v, want 10m", cfg.CIPollMaxBudget)
	}
}

func TestLoadCIMaxFixAttemptsFallback(t *testing.T) {
	for _, value := range []string{"abc", "0", "-1", ""} {
		cfg, err := Load(fullEnv(map[string]string{"CI_MAX_FIX_ATTEMPTS": value}))
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", value, err)
		}
		if cfg.CIMaxFixAttempts != 2 {
			t.Errorf("CI_MAX_FIX_ATTEMPTS=%q → %d, want default 2", value, cfg.CIMaxFixAttempts)
		}
	}
}
