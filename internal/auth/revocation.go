package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/Domene99/drun-dash/internal/redis"
)

// Revoker is the revocation channel a [Verifier] consults after a token's
// signature and claims check out (ADR §5). A token is admitted only if its jti
// is not revoked.
//
// Implementations must be safe for concurrent use. IsRevoked should report an
// error rather than a false negative when it cannot reach its backing store, so
// the verifier can fail closed.
type Revoker interface {
	// Revoke marks jti as revoked for at least ttl. ttl should be the token's
	// remaining lifetime so the entry self-expires once the token would have
	// expired anyway (ADR §3).
	Revoke(ctx context.Context, jti string, ttl time.Duration) error
	// IsRevoked reports whether jti is currently on the denylist.
	IsRevoked(ctx context.Context, jti string) (bool, error)
}

// DefaultRevocationPrefix namespaces revocation denylist keys in Redis. Keys
// are of the form "<prefix><jti>".
const DefaultRevocationPrefix = "authz:revoked:jti:"

// revokedValue is the placeholder stored at a denylist key; only the key's
// existence matters, so the value is a constant.
const revokedValue = "1"

// RedisRevoker is the Redis-backed [Revoker] (ADR §3). Each revoked jti is a
// Redis key with a TTL, so entries self-expire once the corresponding token
// would have expired and the denylist stays small. It uses the shared
// [redis.Client] and only its promoted Set/Exists commands.
type RedisRevoker struct {
	client *redis.Client
	prefix string
}

// RedisRevokerOption customizes a [RedisRevoker].
type RedisRevokerOption func(*RedisRevoker)

// WithKeyPrefix overrides the denylist key prefix (default
// [DefaultRevocationPrefix]). An empty prefix is ignored.
func WithKeyPrefix(prefix string) RedisRevokerOption {
	return func(r *RedisRevoker) {
		if prefix != "" {
			r.prefix = prefix
		}
	}
}

// NewRedisRevoker returns a [RedisRevoker] backed by client. It errors on a nil
// client.
func NewRedisRevoker(client *redis.Client, opts ...RedisRevokerOption) (*RedisRevoker, error) {
	if client == nil {
		return nil, fmt.Errorf("auth: new redis revoker: nil client")
	}
	r := &RedisRevoker{client: client, prefix: DefaultRevocationPrefix}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// key returns the Redis denylist key for jti.
func (r *RedisRevoker) key(jti string) string { return r.prefix + jti }

// Revoke adds jti to the denylist with the given ttl. ttl must be positive;
// revoking with a non-positive ttl is refused because a zero ttl would create a
// key that never expires (go-redis treats 0 as no-expiry), which would leak the
// denylist. When ttl is a token's remaining lifetime, the entry expires exactly
// as the token would have.
func (r *RedisRevoker) Revoke(ctx context.Context, jti string, ttl time.Duration) error {
	if jti == "" {
		return fmt.Errorf("auth: revoke: empty jti")
	}
	if ttl <= 0 {
		return fmt.Errorf("auth: revoke: ttl must be positive, got %s", ttl)
	}
	if err := r.client.Set(ctx, r.key(jti), revokedValue, ttl).Err(); err != nil {
		return fmt.Errorf("auth: revoke: %w", err)
	}
	return nil
}

// IsRevoked reports whether jti is on the denylist. A Redis error is returned
// (not swallowed) so the verifier can fail closed.
func (r *RedisRevoker) IsRevoked(ctx context.Context, jti string) (bool, error) {
	if jti == "" {
		return false, fmt.Errorf("auth: is revoked: empty jti")
	}
	n, err := r.client.Exists(ctx, r.key(jti)).Result()
	if err != nil {
		return false, fmt.Errorf("auth: is revoked: %w", err)
	}
	return n > 0, nil
}
