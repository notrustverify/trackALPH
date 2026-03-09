package store

import (
	"context"
	"log"
	"strconv"
	"strings"

	"trackalph.app/internal/models"

	"github.com/redis/go-redis/v9"
)

const (
	keyWatched    = "trackalph:watched"
	keyAddrPrefix = "trackalph:addr:"
	keyUserPrefix = "trackalph:user:"
	keyWhPrefix   = "trackalph:userwh:"

	FilterAll = "all"
	FilterIn  = "in"
	FilterOut = "out"

	// Field prefixes in the addr hash to distinguish subscriber types.
	telegramFieldPrefix = "t:"
	webhookFieldPrefix  = "w:"
)

type Subscriber struct {
	Channel string
	ChatID  int64
	URL     string
	Filter  string
}

type Subscription struct {
	Address string
	Filter  string
}

type WebhookSubscription struct {
	Address string
	URL     string
	Filter  string
}

type Store struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Store {
	st := &Store{rdb: rdb}
	st.migrateKeys(context.Background())
	return st
}

// --- Telegram subscriptions ---

func (s *Store) AddSubscription(ctx context.Context, chatID int64, address, filter string) error {
	if filter == "" {
		filter = FilterAll
	}
	chatStr := strconv.FormatInt(chatID, 10)
	field := telegramFieldPrefix + chatStr
	pipe := s.rdb.Pipeline()
	pipe.SAdd(ctx, keyWatched, address)
	pipe.HSet(ctx, keyAddrPrefix+address, field, filter)
	pipe.HSet(ctx, keyUserPrefix+chatStr, address, filter)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) RemoveSubscription(ctx context.Context, chatID int64, address string) error {
	chatStr := strconv.FormatInt(chatID, 10)
	field := telegramFieldPrefix + chatStr
	pipe := s.rdb.Pipeline()
	pipe.HDel(ctx, keyAddrPrefix+address, field)
	pipe.HDel(ctx, keyUserPrefix+chatStr, address)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return err
	}
	s.cleanupAddressIfEmpty(ctx, address)
	return nil
}

func (s *Store) GetSubscriptionsForChatID(ctx context.Context, chatID int64) ([]Subscription, error) {
	chatStr := strconv.FormatInt(chatID, 10)
	entries, err := s.rdb.HGetAll(ctx, keyUserPrefix+chatStr).Result()
	if err != nil {
		return nil, err
	}
	var subs []Subscription
	for addr, filter := range entries {
		subs = append(subs, Subscription{Address: addr, Filter: filter})
	}
	return subs, nil
}

// --- Webhook subscriptions ---

func (s *Store) AddWebhook(ctx context.Context, chatID int64, address, url, filter string) error {
	if filter == "" {
		filter = FilterAll
	}
	chatStr := strconv.FormatInt(chatID, 10)
	field := webhookFieldPrefix + url
	pipe := s.rdb.Pipeline()
	pipe.SAdd(ctx, keyWatched, address)
	pipe.HSet(ctx, keyAddrPrefix+address, field, filter)
	pipe.HSet(ctx, keyWhPrefix+chatStr, address, url+"|"+filter)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) RemoveWebhook(ctx context.Context, chatID int64, address string) error {
	chatStr := strconv.FormatInt(chatID, 10)

	// Look up the URL from the user's webhook hash to know which addr field to remove.
	val, err := s.rdb.HGet(ctx, keyWhPrefix+chatStr, address).Result()
	if err != nil {
		return err
	}
	url, _, _ := strings.Cut(val, "|")

	field := webhookFieldPrefix + url
	pipe := s.rdb.Pipeline()
	pipe.HDel(ctx, keyAddrPrefix+address, field)
	pipe.HDel(ctx, keyWhPrefix+chatStr, address)
	_, err = pipe.Exec(ctx)
	if err != nil {
		return err
	}
	s.cleanupAddressIfEmpty(ctx, address)
	return nil
}

func (s *Store) GetWebhooksForChatID(ctx context.Context, chatID int64) ([]WebhookSubscription, error) {
	chatStr := strconv.FormatInt(chatID, 10)
	entries, err := s.rdb.HGetAll(ctx, keyWhPrefix+chatStr).Result()
	if err != nil {
		return nil, err
	}
	var subs []WebhookSubscription
	for addr, val := range entries {
		url, filter, _ := strings.Cut(val, "|")
		subs = append(subs, WebhookSubscription{Address: addr, URL: url, Filter: filter})
	}
	return subs, nil
}

// --- Shared ---

func (s *Store) IsWatched(ctx context.Context, address string) bool {
	ok, _ := s.rdb.SIsMember(ctx, keyWatched, address).Result()
	return ok
}

// GetSubscribersForAddress returns all subscribers (Telegram + webhook) for an address.
func (s *Store) GetSubscribersForAddress(ctx context.Context, address string) []Subscriber {
	entries, _ := s.rdb.HGetAll(ctx, keyAddrPrefix+address).Result()
	var subs []Subscriber
	for field, filter := range entries {
		if rest, ok := strings.CutPrefix(field, telegramFieldPrefix); ok {
			id, err := strconv.ParseInt(rest, 10, 64)
			if err == nil {
				subs = append(subs, Subscriber{
					Channel: models.ChannelTelegram,
					ChatID:  id,
					Filter:  filter,
				})
			}
		} else if rest, ok := strings.CutPrefix(field, webhookFieldPrefix); ok {
			subs = append(subs, Subscriber{
				Channel: models.ChannelWebhook,
				URL:     rest,
				Filter:  filter,
			})
		}
	}
	return subs
}

func (s *Store) cleanupAddressIfEmpty(ctx context.Context, address string) {
	count, err := s.rdb.HLen(ctx, keyAddrPrefix+address).Result()
	if err != nil {
		return
	}
	if count == 0 {
		s.rdb.SRem(ctx, keyWatched, address)
		s.rdb.Del(ctx, keyAddrPrefix+address)
	}
}

// --- Migration ---

// migrateKeys converts old-format keys to the new prefixed format.
// Old format: addr hash fields were bare chatIDs (e.g. "12345").
// New format: prefixed with "t:" (e.g. "t:12345").
func (s *Store) migrateKeys(ctx context.Context) {
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, keyAddrPrefix+"*", 100).Result()
		if err != nil {
			break
		}
		for _, key := range keys {
			s.migrateAddrKey(ctx, key)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
}

func (s *Store) migrateAddrKey(ctx context.Context, key string) {
	t, err := s.rdb.Type(ctx, key).Result()
	if err != nil {
		return
	}

	// Old Set format -> convert to Hash with "t:" prefix
	if t == "set" {
		members, err := s.rdb.SMembers(ctx, key).Result()
		if err != nil || len(members) == 0 {
			return
		}
		fields := make(map[string]interface{}, len(members))
		for _, m := range members {
			fields[telegramFieldPrefix+m] = FilterAll
		}
		pipe := s.rdb.Pipeline()
		pipe.Del(ctx, key)
		pipe.HSet(ctx, key, fields)
		if _, err := pipe.Exec(ctx); err != nil {
			log.Printf("migration: failed to convert set key %s: %v", key, err)
		} else {
			log.Printf("migration: converted set key %s (%d entries)", key, len(members))
		}
		return
	}

	// Old Hash format (bare chatIDs) -> add "t:" prefix
	if t == "hash" {
		entries, err := s.rdb.HGetAll(ctx, key).Result()
		if err != nil || len(entries) == 0 {
			return
		}
		needsMigration := false
		for field := range entries {
			if !strings.HasPrefix(field, telegramFieldPrefix) && !strings.HasPrefix(field, webhookFieldPrefix) {
				needsMigration = true
				break
			}
		}
		if !needsMigration {
			return
		}
		pipe := s.rdb.Pipeline()
		for field, val := range entries {
			if strings.HasPrefix(field, telegramFieldPrefix) || strings.HasPrefix(field, webhookFieldPrefix) {
				continue
			}
			pipe.HDel(ctx, key, field)
			pipe.HSet(ctx, key, telegramFieldPrefix+field, val)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			log.Printf("migration: failed to prefix hash key %s: %v", key, err)
		} else {
			log.Printf("migration: prefixed hash key %s", key)
		}
	}
}
