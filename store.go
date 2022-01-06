package otpgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/knadh/otpgateway/models"
)

// ErrNotExist is thrown when an OTP (requested by namespace / ID)
// does not exist.
var ErrNotExist = errors.New("the OTP does not exist")

// Store represents a storage backend where OTP data is stored.
type Store interface {
	// Set sets an OTP against an ID. Every Set() increments the attempts
	// count against the ID that was initially set.
	Set(namespace, id string, otp models.OTP) (models.OTP, error)

	// SetAddress sets (updates) the address on an existing OTP.
	SetAddress(namespace, id, address string) error

	// Check checks the attempt count and TTL duration against an ID.
	// Passing counter=true increments the attempt counter.
	Check(namespace, id string, counter bool) (models.OTP, error)

	// Close closes an OTP and marks it as done (verified).
	// After this, the OTP has to expire after a TTL or be deleted.
	Close(namespace, id string) error

	// Delete deletes the OTP saved against a given ID.
	Delete(namespace, id string) error

	// Ping checks if store is reachable
	Ping() error
}

// redisStore implements a  Redis Store.
type redisStore struct {
	pool *redis.Pool
	conf RedisConf
}

// RedisConf contains Redis configuration fields.
type RedisConf struct {
	Host      string        `json:"host"`
	Port      int           `json:"port"`
	Username  string        `json:"username"`
	Password  string        `json:"password"`
	DB        int           `json:"db"`
	MaxActive int           `json:"max_active"`
	MaxIdle   int           `json:"max_idle"`
	Timeout   time.Duration `json:"timeout"`
	KeyPrefix string        `json:"key_prefix"`

	// If this is set, 'check' and 'close' events will be PUBLISHed to
	// to this Redis key (Redis PubSub).
	PublishKey string `json:"publish_key"`
}

type event struct {
	Type string `json:"type"`

	Namespace string          `json:"namespace"`
	ID        string          `json:"id"`
	Data      json.RawMessage `json:"data"`
}

// NewRedisStore returns a Redis implementation of store.
func NewRedisStore(c RedisConf) Store {
	if c.KeyPrefix == "" {
		c.KeyPrefix = "OTP"
	}
	pool := &redis.Pool{
		Wait:      true,
		MaxActive: c.MaxActive,
		MaxIdle:   c.MaxIdle,
		Dial: func() (redis.Conn, error) {
			c, err := redis.Dial(
				"tcp",
				fmt.Sprintf("%s:%d", c.Host, c.Port),
				redis.DialPassword(c.Password),
				redis.DialConnectTimeout(c.Timeout),
				redis.DialReadTimeout(c.Timeout),
				redis.DialWriteTimeout(c.Timeout),
				redis.DialDatabase(c.DB),
			)

			return c, err
		},
	}
	return &redisStore{
		conf: c,
		pool: pool,
	}
}

// Ping checks if Redis server is reachable
func (r *redisStore) Ping() error {
	c := r.pool.Get()
	defer c.Close()
	_, err := c.Do("PING") // Test redis connection
	return err
}

// Check checks the attempt count and TTL duration against an ID.
// Passing count=true increments the attempt counter.
func (r *redisStore) Check(namespace, id string, counter bool) (models.OTP, error) {
	c := r.pool.Get()
	defer c.Close()

	out, err := r.get(namespace, id, c)
	if err != nil {
		return out, err
	}
	if !counter {
		return out, err
	}

	// Increment attempts.
	key := r.makeKey(namespace, id)
	r.begin(c)
	c.Send("HINCRBY", key, "attempts", 1)
	c.Send("TTL", key)
	resp, err := r.end(c)
	if err != nil {
		return out, err
	}

	attempts, _ := redis.Int(resp[0], nil)
	out.Attempts = attempts

	ttl, _ := redis.Int64(resp[1], nil)
	out.TTL = time.Duration(ttl) * time.Second

	// Publish?
	if r.conf.PublishKey != "" {
		b, _ := json.Marshal(out)
		e, _ := json.Marshal(event{
			Type:      "check",
			Namespace: namespace,
			ID:        id,
			Data:      json.RawMessage(b),
		})
		_, _ = c.Do("PUBLISH", r.conf.PublishKey, e)
	}

	return out, err
}

// Set sets an OTP in the store.
func (r *redisStore) Set(namespace, id string, otp models.OTP) (models.OTP, error) {
	c := r.pool.Get()
	defer c.Close()

	// Set the OTP value.
	var (
		key = r.makeKey(namespace, id)
		exp = otp.TTL.Nanoseconds() / int64(time.Millisecond)
	)

	r.begin(c)
	c.Send("HMSET", key,
		"otp", otp.OTP,
		"to", otp.To,
		"channel_description", otp.ChannelDesc,
		"address_description", otp.AddressDesc,
		"extra", string(otp.Extra),
		"provider", otp.Provider,
		"closed", false,
		"max_attempts", otp.MaxAttempts)
	c.Send("HINCRBY", key, "attempts", 1)
	c.Send("PEXPIRE", key, exp)

	// Flush the commands and get their responses.
	// [1] is the number of attempts.
	// [3] is the TTL.
	resp, err := r.end(c)
	if err != nil {
		return otp, err
	}
	attempts, err := redis.Int(resp[1], nil)
	if err != nil {
		return otp, err
	}
	otp.Attempts = attempts
	otp.TTLSeconds = otp.TTL.Seconds()
	otp.Namespace = namespace
	otp.ID = id
	return otp, nil
}

// SetAddress sets (updates) the address on an existing OTP.
func (r *redisStore) SetAddress(namespace, id, address string) error {
	c := r.pool.Get()
	defer c.Close()

	// Set the OTP value.
	var key = r.makeKey(namespace, id)

	if _, err := c.Do("HSET", key, "to", address); err != nil {
		return err
	}

	return nil
}

// Close closes an OTP and marks it as done (verified).
// After this, the OTP has to expire after a TTL or be deleted.
func (r *redisStore) Close(namespace, id string) error {
	c := r.pool.Get()
	defer c.Close()

	_, err := c.Do("HSET", r.makeKey(namespace, id), "closed", true)

	// Publish?
	if r.conf.PublishKey != "" {
		e, _ := json.Marshal(event{
			Type:      "close",
			Namespace: namespace,
			ID:        id,
			Data:      json.RawMessage([]byte(`null`)),
		})
		_, _ = c.Do("PUBLISH", r.conf.PublishKey, e)
	}

	return err
}

// Delete deletes the OTP saved against a given ID.
func (r *redisStore) Delete(namespace, id string) error {
	c := r.pool.Get()
	defer c.Close()

	_, err := c.Do("DEL", r.makeKey(namespace, id))
	return err
}

// get begins a transaction.
func (r *redisStore) get(namespace, id string, c redis.Conn) (models.OTP, error) {
	var (
		key = r.makeKey(namespace, id)
		out = models.OTP{
			Namespace: namespace,
			ID:        id,
		}
	)

	resp, err := redis.Values(c.Do("HGETALL", key))
	if err != nil {
		return out, err
	}
	if err := redis.ScanStruct(resp, &out); err != nil {
		return out, err
	}

	// Doesn't exist?
	if out.OTP == "" {
		return out, ErrNotExist
	}

	ttl, err := redis.Int64(c.Do("TTL", key))
	if err != nil {
		return out, err
	}

	out.TTL = time.Duration(ttl) * time.Second
	out.TTLSeconds = out.TTL.Seconds()
	return out, nil
}

// begin begins a transaction.
func (r *redisStore) begin(c redis.Conn) error {
	return c.Send("MULTI")
}

// end begins a transaction.
func (r *redisStore) end(c redis.Conn) ([]interface{}, error) {
	rep, err := redis.Values(c.Do("EXEC"))

	// Check if there are any errors.
	for _, r := range rep {
		if v, ok := r.(redis.Error); ok {
			return rep, v
		}
	}
	return rep, err
}

// makeKey makes the Redis key for the OTP.
func (r *redisStore) makeKey(namespace, id string) string {
	return fmt.Sprintf("%s:%s:%s", r.conf.KeyPrefix, namespace, id)
}
