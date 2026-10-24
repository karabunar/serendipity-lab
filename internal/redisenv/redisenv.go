// Package redisenv builds services' Redis client from environment variables.
package redisenv

import (
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/envutil"
)

// New builds a client from REDIS_ADDR (default localhost:6379) and REDIS_PASSWORD.
func New() *goredis.Client {
	return goredis.NewClient(&goredis.Options{
		Addr:     envutil.String("REDIS_ADDR", "localhost:6379"),
		Password: envutil.String("REDIS_PASSWORD", ""),
	})
}
