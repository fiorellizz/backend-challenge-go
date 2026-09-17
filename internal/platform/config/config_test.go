package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

func minimal() map[string]string {
	return map[string]string{
		"DATABASE_URL":         "postgres://wager:wager@localhost:5433/wager?sslmode=disable",
		"SQS_WAGER_QUEUE_URL":  "http://localhost:4566/000000000000/wager-transactions.fifo",
		"SQS_WAGER_DLQ_URL":    "http://localhost:4566/000000000000/wager-transactions-dlq.fifo",
		"SQS_EVENTS_QUEUE_URL": "http://localhost:4566/000000000000/wager-events.fifo",
		"OIDC_ISSUER_URL":      "http://localhost:8080/realms/wager",
	}
}

func getenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := config.Load(getenv(minimal()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceID != "app" || cfg.HTTPAddr != ":8080" || cfg.ShutdownTimeout != 20*time.Second {
		t.Errorf("process defaults: %+v", cfg)
	}
	if !cfg.Roles.Has(config.RoleAPI) || !cfg.Roles.Has(config.RoleConsumer) || !cfg.Roles.Has(config.RoleOutbox) {
		t.Errorf("all roles enabled by default: %s", cfg.Roles)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "json" {
		t.Errorf("log defaults: %+v", cfg.Log)
	}
	if cfg.Database.MaxConns != 10 || cfg.SQS.MaxMessages != 10 || cfg.SQS.VisibilityTimeoutSeconds != 30 {
		t.Errorf("numeric defaults: %+v %+v", cfg.Database, cfg.SQS)
	}
	if cfg.OIDC.Audience != "wager-api" || cfg.OIDC.ProviderClaim != "provider_id" {
		t.Errorf("oidc defaults: %+v", cfg.OIDC)
	}
	if cfg.Outbox.PollInterval != 500*time.Millisecond || cfg.Reference.TTL != 5*time.Minute {
		t.Errorf("worker defaults: %+v %+v", cfg.Outbox, cfg.Reference)
	}
}

func TestLoadParsesRolesDurationsAndIntegers(t *testing.T) {
	env := minimal()
	env["APP_ROLES"] = " API , outbox"
	env["SHUTDOWN_TIMEOUT"] = "5s"
	env["DATABASE_MAX_CONNS"] = "3"
	env["LOG_FORMAT"] = "TEXT"

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Roles.Has(config.RoleAPI) || cfg.Roles.Has(config.RoleConsumer) || !cfg.Roles.Has(config.RoleOutbox) {
		t.Errorf("roles = %s", cfg.Roles)
	}
	if cfg.Roles.String() != "api,outbox" {
		t.Errorf("roles string = %q", cfg.Roles.String())
	}
	if cfg.ShutdownTimeout != 5*time.Second || cfg.Database.MaxConns != 3 || cfg.Log.Format != "text" {
		t.Errorf("parsed values: %+v", cfg)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown role":        {"APP_ROLES": "api,scheduler"},
		"empty roles":         {"APP_ROLES": ","},
		"bad duration":        {"SHUTDOWN_TIMEOUT": "twenty"},
		"bad integer":         {"DATABASE_MAX_CONNS": "ten"},
		"bad log level":       {"LOG_LEVEL": "verbose"},
		"bad log format":      {"LOG_FORMAT": "xml"},
		"sqs batch too large": {"SQS_MAX_MESSAGES": "11"},
		"sqs wait too long":   {"SQS_WAIT_TIME_SECONDS": "21"},
		"zero outbox batch":   {"OUTBOX_BATCH_SIZE": "0"},
		"zero reference ttl":  {"REFERENCE_TTL": "0s"},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			env := minimal()
			for k, v := range override {
				env[k] = v
			}
			if _, err := config.Load(getenv(env)); err == nil {
				t.Fatalf("expected an error")
			}
		})
	}
}

func TestLoadReportsAllMissingRequiredValuesAtOnce(t *testing.T) {
	_, err := config.Load(getenv(map[string]string{}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"DATABASE_URL", "SQS_WAGER_QUEUE_URL", "SQS_WAGER_DLQ_URL", "SQS_EVENTS_QUEUE_URL", "OIDC_ISSUER_URL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}
