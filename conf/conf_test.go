package conf

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
)

func TestAgentCompatibilityConfig(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		body := ""
		if enabled {
			body = "codex_agent_compatibility:\n  portable_models: [deepseek-flash, deepseek-v4.1-flash]\n"
		}
		cfg, _, err := loadConfig(writeTestConfig(t, body))
		if err != nil {
			t.Fatal(err)
		}
		got := cfg.CodexAgentCompatibility
		if enabled {
			if !slices.Equal(got.PortableModels, []string{"deepseek-flash", "deepseek-v4.1-flash"}) {
				t.Fatalf("unexpected compatibility config: %+v", got)
			}
		} else if len(got.PortableModels) != 0 {
			t.Fatalf("compatibility must default to disabled: %+v", got)
		}
	}
}

func TestAgentCompatibilityEnvironment(t *testing.T) {
	t.Setenv("AXONHUB_CODEX_AGENT_COMPATIBILITY_PORTABLE_MODELS", `["deepseek-flash","deepseek-v4.1-flash"]`)
	cfg, _, err := loadConfig(writeTestConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.CodexAgentCompatibility.PortableModels, []string{"deepseek-flash", "deepseek-v4.1-flash"}) {
		t.Fatalf("environment compatibility config was not loaded: %+v", cfg.CodexAgentCompatibility)
	}
}

func TestLoaderReloadUsesFreshViper(t *testing.T) {
	configFile := writeTestConfig(t, `
log:
  level: info
`)
	loader := &Loader{configFile: configFile}

	initial, err := loader.Reload()
	if err != nil {
		t.Fatalf("initial Reload() error = %v", err)
	}
	if initial.Log.Level != zapcore.InfoLevel {
		t.Fatalf("initial log level = %s, want %s", initial.Log.Level, zapcore.InfoLevel)
	}

	if err := os.WriteFile(configFile, []byte(`
log:
  level: debug
`), 0o600); err != nil {
		t.Fatalf("rewrite test config: %v", err)
	}

	reloaded, err := loader.Reload()
	if err != nil {
		t.Fatalf("reloaded Reload() error = %v", err)
	}
	if reloaded.Log.Level != zapcore.DebugLevel {
		t.Fatalf("reloaded log level = %s, want %s", reloaded.Log.Level, zapcore.DebugLevel)
	}
}

func TestSSEKeepAliveConfig(t *testing.T) {
	configFile := writeTestConfig(t, `
server:
  sse_keep_alive:
    enabled: true
    interval: 30s
`)

	cfg, _, err := loadConfig(configFile)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if !cfg.APIServer.SSEKeepAlive.Enabled {
		t.Fatal("SSE keep-alive should be enabled")
	}
	if cfg.APIServer.SSEKeepAlive.Interval != 30*time.Second {
		t.Fatalf("SSE keep-alive interval = %s, want 30s", cfg.APIServer.SSEKeepAlive.Interval)
	}
}

func TestSSEKeepAliveDefaultsToDisabled(t *testing.T) {
	configFile := writeTestConfig(t, "")

	cfg, _, err := loadConfig(configFile)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.APIServer.SSEKeepAlive.Enabled {
		t.Fatal("SSE keep-alive should default to disabled")
	}
	if cfg.APIServer.SSEKeepAlive.Interval != 15*time.Second {
		t.Fatalf("SSE keep-alive interval = %s, want 15s", cfg.APIServer.SSEKeepAlive.Interval)
	}
}

func TestMetricsExporterConfigFromEnvironment(t *testing.T) {
	t.Setenv("AXONHUB_METRICS_ENABLED", "true")
	t.Setenv("AXONHUB_METRICS_EXPORTER_TYPE", "otlphttp")
	t.Setenv("AXONHUB_METRICS_EXPORTER_ENDPOINT", "alloy.alloy.svc.cluster.local:4318")
	t.Setenv("AXONHUB_METRICS_EXPORTER_INSECURE", "true")

	configFile := writeTestConfig(t, "")
	cfg, _, err := loadConfig(configFile)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if !cfg.Metrics.Enabled {
		t.Fatal("metrics should be enabled")
	}
	if cfg.Metrics.Exporter.Type != "otlphttp" {
		t.Fatalf("metrics exporter type = %q, want %q", cfg.Metrics.Exporter.Type, "otlphttp")
	}
	if cfg.Metrics.Exporter.Endpoint != "alloy.alloy.svc.cluster.local:4318" {
		t.Fatalf(
			"metrics exporter endpoint = %q, want %q",
			cfg.Metrics.Exporter.Endpoint,
			"alloy.alloy.svc.cluster.local:4318",
		)
	}
	if !cfg.Metrics.Exporter.Insecure {
		t.Fatal("metrics exporter should use an insecure connection")
	}
}

func TestTraceExtractionDefaultsToEnabled(t *testing.T) {
	configFile := writeTestConfig(t, "")

	cfg, _, err := loadConfig(configFile)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	trace := cfg.APIServer.Trace
	if !trace.ClaudeCodeTraceEnabled {
		t.Error("Claude Code trace extraction should default to enabled")
	}
	if !trace.CodexTraceEnabled {
		t.Error("Codex trace extraction should default to enabled")
	}
	if !trace.OpenCodeTraceEnabled {
		t.Error("OpenCode trace extraction should default to enabled")
	}
}

func writeTestConfig(t *testing.T, contents string) string {
	t.Helper()

	configFile := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return configFile
}
