// Package config loads and validates the process configuration from
// environment variables. It is built before the Fx container so that
// options such as the shutdown timeout can depend on it.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Role is one of the responsibilities a process can run. One binary serves
// all three; APP_ROLES selects which ones a given instance takes.
type Role string

const (
	RoleAPI      Role = "api"
	RoleConsumer Role = "consumer"
	RoleOutbox   Role = "outbox"
)

// Roles is the set of roles enabled for this instance.
type Roles map[Role]bool

// Has reports whether the role is enabled.
func (r Roles) Has(role Role) bool { return r[role] }

// String renders the enabled roles sorted for logs.
func (r Roles) String() string {
	var out []string
	for _, role := range []Role{RoleAPI, RoleConsumer, RoleOutbox} {
		if r[role] {
			out = append(out, string(role))
		}
	}
	return strings.Join(out, ",")
}

// Config is the full, validated configuration.
type Config struct {
	InstanceID      string
	Roles           Roles
	HTTPAddr        string
	ShutdownTimeout time.Duration
	Log             Log
	Database        Database
	AWS             AWS
	SQS             SQS
	OIDC            OIDC
	Outbox          Outbox
	Reference       Reference
}

type Log struct {
	Level  string // debug | info | warn | error
	Format string // json | text
}

type Database struct {
	URL      string
	MaxConns int32
}

type AWS struct {
	Region   string
	Endpoint string // LocalStack endpoint; empty means the real AWS endpoint
}

type SQS struct {
	WagerQueueURL            string
	WagerDLQURL              string
	EventsQueueURL           string
	MaxMessages              int32
	WaitTimeSeconds          int32
	VisibilityTimeoutSeconds int32
}

type OIDC struct {
	IssuerURL     string // value expected in the token's iss claim
	DiscoveryURL  string // where to fetch .well-known and JWKS; defaults to IssuerURL
	Audience      string
	InternalRole  string
	ProviderRole  string
	ProviderClaim string
}

type Outbox struct {
	BatchSize    int
	PollInterval time.Duration
	MaxAttempts  int
}

type Reference struct {
	PollInterval time.Duration
	BaseBackoff  time.Duration
	MaxAttempts  int
	TTL          time.Duration
}

// Load reads every variable through getenv (os.Getenv in production, a map
// in tests), applies defaults for optional values and validates the result.
func Load(getenv func(string) string) (Config, error) {
	r := reader{getenv: getenv}

	cfg := Config{
		InstanceID:      r.str("APP_INSTANCE_ID", "app"),
		Roles:           r.roles("APP_ROLES", "api,consumer,outbox"),
		HTTPAddr:        r.str("HTTP_ADDR", ":8080"),
		ShutdownTimeout: r.duration("SHUTDOWN_TIMEOUT", 20*time.Second),
		Log: Log{
			Level:  strings.ToLower(r.str("LOG_LEVEL", "info")),
			Format: strings.ToLower(r.str("LOG_FORMAT", "json")),
		},
		Database: Database{
			URL:      r.str("DATABASE_URL", ""),
			MaxConns: int32(r.integer("DATABASE_MAX_CONNS", 10)),
		},
		AWS: AWS{
			Region:   r.str("AWS_REGION", "us-east-1"),
			Endpoint: r.str("AWS_ENDPOINT_URL", ""),
		},
		SQS: SQS{
			WagerQueueURL:            r.str("SQS_WAGER_QUEUE_URL", ""),
			WagerDLQURL:              r.str("SQS_WAGER_DLQ_URL", ""),
			EventsQueueURL:           r.str("SQS_EVENTS_QUEUE_URL", ""),
			MaxMessages:              int32(r.integer("SQS_MAX_MESSAGES", 10)),
			WaitTimeSeconds:          int32(r.integer("SQS_WAIT_TIME_SECONDS", 10)),
			VisibilityTimeoutSeconds: int32(r.integer("SQS_VISIBILITY_TIMEOUT_SECONDS", 30)),
		},
		OIDC: OIDC{
			IssuerURL:     r.str("OIDC_ISSUER_URL", ""),
			DiscoveryURL:  r.str("OIDC_DISCOVERY_URL", r.str("OIDC_ISSUER_URL", "")),
			Audience:      r.str("OIDC_AUDIENCE", "wager-api"),
			InternalRole:  r.str("OIDC_INTERNAL_ROLE", "internal-service"),
			ProviderRole:  r.str("OIDC_PROVIDER_ROLE", "provider"),
			ProviderClaim: r.str("OIDC_PROVIDER_CLAIM", "provider_id"),
		},
		Outbox: Outbox{
			BatchSize:    r.integer("OUTBOX_BATCH_SIZE", 100),
			PollInterval: r.duration("OUTBOX_POLL_INTERVAL", 500*time.Millisecond),
			MaxAttempts:  r.integer("OUTBOX_MAX_ATTEMPTS", 10),
		},
		Reference: Reference{
			PollInterval: r.duration("REFERENCE_POLL_INTERVAL", time.Second),
			BaseBackoff:  r.duration("REFERENCE_BASE_BACKOFF", 2*time.Second),
			MaxAttempts:  r.integer("REFERENCE_MAX_ATTEMPTS", 8),
			TTL:          r.duration("REFERENCE_TTL", 5*time.Minute),
		},
	}
	if r.err != nil {
		return Config{}, r.err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks the rules that a running process depends on. It reports
// every problem at once so a misconfigured deploy is fixed in one round.
func (c Config) Validate() error {
	var problems []error
	require := func(ok bool, format string, args ...any) {
		if !ok {
			problems = append(problems, fmt.Errorf(format, args...))
		}
	}

	require(len(c.Roles) > 0, "APP_ROLES must enable at least one role")
	require(c.HTTPAddr != "", "HTTP_ADDR is required")
	require(c.ShutdownTimeout > 0, "SHUTDOWN_TIMEOUT must be positive")
	require(c.Log.Level == "debug" || c.Log.Level == "info" || c.Log.Level == "warn" || c.Log.Level == "error",
		"LOG_LEVEL must be debug, info, warn or error")
	require(c.Log.Format == "json" || c.Log.Format == "text", "LOG_FORMAT must be json or text")
	require(c.Database.URL != "", "DATABASE_URL is required")
	require(c.Database.MaxConns > 0, "DATABASE_MAX_CONNS must be positive")
	require(c.AWS.Region != "", "AWS_REGION is required")
	require(c.SQS.WagerQueueURL != "", "SQS_WAGER_QUEUE_URL is required")
	require(c.SQS.WagerDLQURL != "", "SQS_WAGER_DLQ_URL is required")
	require(c.SQS.EventsQueueURL != "", "SQS_EVENTS_QUEUE_URL is required")
	require(c.SQS.MaxMessages >= 1 && c.SQS.MaxMessages <= 10, "SQS_MAX_MESSAGES must be between 1 and 10")
	require(c.SQS.WaitTimeSeconds >= 0 && c.SQS.WaitTimeSeconds <= 20, "SQS_WAIT_TIME_SECONDS must be between 0 and 20")
	require(c.SQS.VisibilityTimeoutSeconds > 0, "SQS_VISIBILITY_TIMEOUT_SECONDS must be positive")
	require(c.OIDC.IssuerURL != "", "OIDC_ISSUER_URL is required")
	require(c.OIDC.Audience != "", "OIDC_AUDIENCE is required")
	require(c.OIDC.InternalRole != "" && c.OIDC.ProviderRole != "", "OIDC roles are required")
	require(c.OIDC.ProviderClaim != "", "OIDC_PROVIDER_CLAIM is required")
	require(c.Outbox.BatchSize > 0 && c.Outbox.PollInterval > 0 && c.Outbox.MaxAttempts > 0,
		"OUTBOX_BATCH_SIZE, OUTBOX_POLL_INTERVAL and OUTBOX_MAX_ATTEMPTS must be positive")
	require(c.Reference.PollInterval > 0 && c.Reference.BaseBackoff > 0 && c.Reference.MaxAttempts > 0 && c.Reference.TTL > 0,
		"REFERENCE_POLL_INTERVAL, REFERENCE_BASE_BACKOFF, REFERENCE_MAX_ATTEMPTS and REFERENCE_TTL must be positive")

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration: %w", errors.Join(problems...))
	}
	return nil
}

// reader accumulates the first parse error instead of stopping, so Load can
// still report validation problems for the other fields.
type reader struct {
	getenv func(string) string
	err    error
}

func (r *reader) str(key, def string) string {
	if v := strings.TrimSpace(r.getenv(key)); v != "" {
		return v
	}
	return def
}

func (r *reader) integer(key string, def int) int {
	v := strings.TrimSpace(r.getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.fail("%s must be an integer, got %q", key, v)
		return def
	}
	return n
}

func (r *reader) duration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(r.getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.fail("%s must be a duration such as 500ms or 20s, got %q", key, v)
		return def
	}
	return d
}

func (r *reader) roles(key, def string) Roles {
	roles := Roles{}
	for _, raw := range strings.Split(r.str(key, def), ",") {
		role := Role(strings.ToLower(strings.TrimSpace(raw)))
		switch role {
		case RoleAPI, RoleConsumer, RoleOutbox:
			roles[role] = true
		case "":
		default:
			r.fail("%s contains unknown role %q", key, raw)
		}
	}
	return roles
}

func (r *reader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("invalid configuration: "+format, args...)
	}
}
