package cache

import (
	"context"

	"github.com/redis/go-redis/v9"
)

func NewRedis(ctx context.Context, addr string) (*redis.Client, error) {
	c := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: 1})
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}
