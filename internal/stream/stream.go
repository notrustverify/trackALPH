package stream

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	BlocksStream        = "trackalph:blocks"
	EthBlocksStream     = "trackalph:eth:blocks"
	NotificationsStream = "trackalph:notifications"
)

type Client struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Client {
	return &Client{rdb: rdb}
}

func (c *Client) Publish(ctx context.Context, streamKey string, data []byte) error {
	return c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		Values: map[string]interface{}{"data": string(data)},
		MaxLen: 10000,
		Approx: true,
	}).Err()
}

func (c *Client) CreateGroup(ctx context.Context, streamKey, group string) error {
	err := c.rdb.XGroupCreateMkStream(ctx, streamKey, group, "0").Err()
	if err != nil && err.Error() == "BUSYGROUP Consumer Group name already exists" {
		return nil
	}
	return err
}

func (c *Client) Consume(ctx context.Context, streamKey, group, consumer string) ([]redis.XMessage, error) {
	results, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{streamKey, ">"},
		Count:    10,
		Block:    5 * time.Second,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results[0].Messages, nil
}

func (c *Client) Ack(ctx context.Context, streamKey, group string, ids ...string) error {
	return c.rdb.XAck(ctx, streamKey, group, ids...).Err()
}
