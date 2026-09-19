package relayhub

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisReplay is the main's ReplayStore: SET NX EX on the shared local Redis,
// so a captured request cannot be replayed even across a hub restart.
type RedisReplay struct{ Client *redis.Client }

func (r *RedisReplay) Seen(ctx context.Context, relayID string, nonce []byte, ttl time.Duration) (bool, error) {
	ok, err := r.Client.SetNX(ctx, "relaylink:nonce:"+relayID+":"+hex.EncodeToString(nonce), 1, ttl).Result()
	if err != nil {
		return false, err
	}
	return !ok, nil // SetNX true = first time seen
}
