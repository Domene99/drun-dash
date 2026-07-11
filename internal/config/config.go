// Package config loads drun-dash runtime configuration from the process
// environment.
//
// Every drun-dash binary (control-plane, llm-proxy, and the CLI) reads the
// same [Config] struct at startup via [Load]. Configuration comes exclusively
// from environment variables; each field has a sensible default tuned for the
// local Docker dev stack (see deploy/docker), so a fresh checkout runs with no
// environment set. Values are validated eagerly in [Load] so misconfiguration
// fails fast at boot rather than on first use.
//
// This package only reads and validates configuration. It does not open
// database or Redis connections, and it holds no secrets/vault logic.
package config

// Default values match the local Docker dev stack documented in
// deploy/docker/README.md and deploy/docker/.env.example. They are safe for
// local development only — production deployments must set every variable
// explicitly.
const (
	// DefaultDatabaseURL points at the local Postgres container. sslmode is
	// disabled because the dev container serves no TLS; never use this against
	// a production database.
	DefaultDatabaseURL = "postgres://drundash:drundash@localhost:5432/drundash?sslmode=disable"
	// DefaultRedisURL points at the local Redis container, logical database 0.
	DefaultRedisURL = "redis://localhost:6379/0"
	// DefaultHTTPAddr is the control-plane HTTP listen address.
	DefaultHTTPAddr = ":8080"
	// DefaultProxyHTTPAddr is the llm-proxy HTTP listen address.
	DefaultProxyHTTPAddr = ":8081"
	// DefaultLogLevel is the default structured-log verbosity.
	DefaultLogLevel = "info"
	// DefaultEnv is the default deployment environment.
	DefaultEnv = EnvDevelopment
)

// Environment names for the ENV variable.
const (
	EnvDevelopment = "development"
	EnvStaging     = "staging"
	EnvProduction  = "production"
)

// Log levels for the LOG_LEVEL variable.
const (
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
)

// Config is the fully-resolved runtime configuration shared by every
// drun-dash binary. Construct it with [Load]; the zero value is not usable.
//
// The doc comment on each field names its environment variable, its default,
// and the binaries that consume it.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string.
	//
	// Env: DATABASE_URL. Default: DefaultDatabaseURL. Used by: control-plane.
	DatabaseURL string

	// RedisURL is the Redis connection string.
	//
	// Env: REDIS_URL. Default: DefaultRedisURL. Used by: control-plane,
	// llm-proxy.
	RedisURL string

	// HTTPAddr is the control-plane HTTP listen address (host:port; host may
	// be empty to listen on all interfaces).
	//
	// Env: HTTP_ADDR. Default: DefaultHTTPAddr. Used by: control-plane.
	HTTPAddr string

	// ProxyHTTPAddr is the llm-proxy HTTP listen address (host:port).
	//
	// Env: PROXY_HTTP_ADDR. Default: DefaultProxyHTTPAddr. Used by: llm-proxy.
	ProxyHTTPAddr string

	// LogLevel is the structured-log verbosity: one of debug, info, warn,
	// error.
	//
	// Env: LOG_LEVEL. Default: DefaultLogLevel. Used by: all.
	LogLevel string

	// Env is the deployment environment: one of development, staging,
	// production.
	//
	// Env: ENV. Default: DefaultEnv. Used by: all.
	Env string
}

// IsProduction reports whether the config targets the production environment.
func (c *Config) IsProduction() bool { return c.Env == EnvProduction }
