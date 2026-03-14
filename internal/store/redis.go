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
	keyLabelPrefix = "trackalph:userlabel:"
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
	Label   string
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

func (s *Store) AddSubscription(ctx context.Context, chatID int64, address, filter, label string) error {
	if filter == "" {
		filter = FilterAll
	}
	chatStr := strconv.FormatInt(chatID, 10)
	field := telegramFieldPrefix + chatStr
	pipe := s.rdb.Pipeline()
	pipe.SAdd(ctx, keyWatched, address)
	pipe.HSet(ctx, keyAddrPrefix+address, field, filter)
	pipe.HSet(ctx, keyUserPrefix+chatStr, address, filter)
	if label != "" {
		pipe.HSet(ctx, keyLabelPrefix+chatStr, address, label)
	} else {
		pipe.HDel(ctx, keyLabelPrefix+chatStr, address)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) RemoveSubscription(ctx context.Context, chatID int64, address string) error {
	chatStr := strconv.FormatInt(chatID, 10)
	field := telegramFieldPrefix + chatStr
	pipe := s.rdb.Pipeline()
	pipe.HDel(ctx, keyAddrPrefix+address, field)
	pipe.HDel(ctx, keyUserPrefix+chatStr, address)
	pipe.HDel(ctx, keyLabelPrefix+chatStr, address)
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
	labels, _ := s.rdb.HGetAll(ctx, keyLabelPrefix+chatStr).Result()
	var subs []Subscription
	for addr, filter := range entries {
		subs = append(subs, Subscription{Address: addr, Filter: filter, Label: labels[addr]})
	}
	return subs, nil
}

func (s *Store) GetAddressLabel(ctx context.Context, chatID int64, address string) string {
	chatStr := strconv.FormatInt(chatID, 10)
	val, err := s.rdb.HGet(ctx, keyLabelPrefix+chatStr, address).Result()
	if err != nil {
		return ""
	}
	return val
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

// CountWatchedAddresses returns how many unique addresses are tracked.
func (s *Store) CountWatchedAddresses(ctx context.Context) int64 {
	n, err := s.rdb.SCard(ctx, keyWatched).Result()
	if err != nil {
		return 0
	}
	return n
}

// CountWatchedAddressesByChannel returns unique address counts split by subscriber channel.
// An address can be counted in both telegram and webhook buckets if it has both.
func (s *Store) CountWatchedAddressesByChannel(ctx context.Context) (total, telegram, webhook int64) {
	total = s.CountWatchedAddresses(ctx)

	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, keyAddrPrefix+"*", 200).Result()
		if err != nil {
			return total, telegram, webhook
		}
		for _, key := range keys {
			fields, err := s.rdb.HKeys(ctx, key).Result()
			if err != nil {
				continue
			}
			hasTelegram := false
			hasWebhook := false
			for _, f := range fields {
				if strings.HasPrefix(f, telegramFieldPrefix) {
					hasTelegram = true
				} else if strings.HasPrefix(f, webhookFieldPrefix) {
					hasWebhook = true
				}
				if hasTelegram && hasWebhook {
					break
				}
			}
			if hasTelegram {
				telegram++
			}
			if hasWebhook {
				webhook++
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	return total, telegram, webhook
}

// CountWatchedAddressesByChain returns unique tracked address counts split by chain/address family.
func (s *Store) CountWatchedAddressesByChain(ctx context.Context) (ethereum, alephium int64) {
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, keyAddrPrefix+"*", 200).Result()
		if err != nil {
			return ethereum, alephium
		}
		for _, key := range keys {
			addr := strings.TrimPrefix(key, keyAddrPrefix)
			if isEthereumAddress(addr) {
				ethereum++
			} else {
				alephium++
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return ethereum, alephium
}

func isEthereumAddress(addr string) bool {
	if len(addr) != 42 || !strings.HasPrefix(addr, "0x") {
		return false
	}
	for _, c := range addr[2:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
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
