package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
)

// Validation sentinel errors. Callers can match specific failure modes with
// errors.Is.
var (
	// ErrMissingField indicates a required field resolved to an empty value
	// (for example, a variable explicitly set to ""). The error message names
	// the environment variable.
	ErrMissingField = errors.New("config: missing required field")
	// ErrInvalidValue indicates a field held a syntactically or semantically
	// invalid value. The error message names the variable and the problem.
	ErrInvalidValue = errors.New("config: invalid value")
)

// allowedLogLevels and allowedEnvs are the accepted enum values for LOG_LEVEL
// and ENV respectively.
var (
	allowedLogLevels = map[string]bool{
		LogLevelDebug: true,
		LogLevelInfo:  true,
		LogLevelWarn:  true,
		LogLevelError: true,
	}
	allowedEnvs = map[string]bool{
		EnvDevelopment: true,
		EnvStaging:     true,
		EnvProduction:  true,
	}
)

// Load reads configuration from the process environment, applying the package
// defaults for any variable that is unset, then validates the result.
//
// An unset variable takes its default; a variable set to the empty string is
// treated as an explicit empty value and fails validation for required fields.
// Load never opens network connections — it only parses and validates.
func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:   getEnv("DATABASE_URL", DefaultDatabaseURL),
		RedisURL:      getEnv("REDIS_URL", DefaultRedisURL),
		HTTPAddr:      getEnv("HTTP_ADDR", DefaultHTTPAddr),
		ProxyHTTPAddr: getEnv("PROXY_HTTP_ADDR", DefaultProxyHTTPAddr),
		LogLevel:      getEnv("LOG_LEVEL", DefaultLogLevel),
		Env:           getEnv("ENV", DefaultEnv),
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate checks that every field is present and well-formed. It is called by
// [Load]; callers constructing a Config by hand can invoke it directly.
//
// It verifies that required fields are non-empty, that DATABASE_URL and
// REDIS_URL are absolute URLs with the expected schemes, that the listen
// addresses are valid host:port pairs, and that LOG_LEVEL and ENV are known
// enum values.
func (c *Config) Validate() error {
	required := []struct {
		env   string
		value string
	}{
		{"DATABASE_URL", c.DatabaseURL},
		{"REDIS_URL", c.RedisURL},
		{"HTTP_ADDR", c.HTTPAddr},
		{"PROXY_HTTP_ADDR", c.ProxyHTTPAddr},
		{"LOG_LEVEL", c.LogLevel},
		{"ENV", c.Env},
	}
	for _, f := range required {
		if f.value == "" {
			return fmt.Errorf("%w: %s", ErrMissingField, f.env)
		}
	}

	if err := validateURL("DATABASE_URL", c.DatabaseURL, "postgres", "postgresql"); err != nil {
		return err
	}
	if err := validateURL("REDIS_URL", c.RedisURL, "redis", "rediss"); err != nil {
		return err
	}
	if err := validateAddr("HTTP_ADDR", c.HTTPAddr); err != nil {
		return err
	}
	if err := validateAddr("PROXY_HTTP_ADDR", c.ProxyHTTPAddr); err != nil {
		return err
	}
	if !allowedLogLevels[c.LogLevel] {
		return fmt.Errorf("%w: LOG_LEVEL=%q (want one of debug, info, warn, error)", ErrInvalidValue, c.LogLevel)
	}
	if !allowedEnvs[c.Env] {
		return fmt.Errorf("%w: ENV=%q (want one of development, staging, production)", ErrInvalidValue, c.Env)
	}
	return nil
}

// getEnv returns the value of key, or fallback if key is unset. A variable set
// to the empty string returns "" so callers can detect explicit clearing.
func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// validateURL checks that raw parses as an absolute URL whose scheme is one of
// schemes and that a host is present.
func validateURL(env, raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %s=%q (%v)", ErrInvalidValue, env, raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: %s=%q (missing host)", ErrInvalidValue, env, raw)
	}
	for _, s := range schemes {
		if u.Scheme == s {
			return nil
		}
	}
	return fmt.Errorf("%w: %s=%q (scheme must be one of %v)", ErrInvalidValue, env, raw, schemes)
}

// validateAddr checks that addr is a valid host:port listen address. An empty
// host (e.g. ":8080") is allowed and binds all interfaces.
func validateAddr(env, addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("%w: %s=%q (want host:port, %v)", ErrInvalidValue, env, addr, err)
	}
	return nil
}
