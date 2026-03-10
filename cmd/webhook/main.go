package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"trackalph.app/internal/config"
	"trackalph.app/internal/models"
	"trackalph.app/internal/stream"

	"github.com/redis/go-redis/v9"
)

const (
	groupName    = "webhook-notifiers"
	consumerName = "webhook-1"
	numWorkers   = 5
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

func main() {
	cfg := config.Load()

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Fatalf("Invalid REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("Redis connection failed: %v", err)
	}

	str := stream.New(rdb)

	if err := str.CreateGroup(context.Background(), stream.NotificationsStream, groupName); err != nil {
		log.Fatalf("Failed to create consumer group: %v", err)
	}

	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deliveryCh := make(chan models.Notification, 200)

	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deliveryWorker(appCtx, deliveryCh)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		consumeNotifications(appCtx, str, deliveryCh)
	}()

	log.Println("Webhook notifier is running")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down webhook notifier...")
	cancel()
	wg.Wait()
	log.Println("Webhook notifier stopped")
}

func consumeNotifications(ctx context.Context, str *stream.Client, ch chan<- models.Notification) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := str.Consume(ctx, stream.NotificationsStream, groupName, consumerName)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("Error consuming notifications: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		for _, msg := range msgs {
			data, ok := msg.Values["data"].(string)
			if !ok {
				str.Ack(ctx, stream.NotificationsStream, groupName, msg.ID)
				continue
			}

			var notif models.Notification
			if err := json.Unmarshal([]byte(data), &notif); err != nil {
				log.Printf("Error unmarshaling notification: %v", err)
				str.Ack(ctx, stream.NotificationsStream, groupName, msg.ID)
				continue
			}

			if notif.Channel == models.ChannelWebhook && notif.URL != "" {
				select {
				case ch <- notif:
				case <-ctx.Done():
					return
				}
			}
			str.Ack(ctx, stream.NotificationsStream, groupName, msg.ID)
		}
	}
}

func deliveryWorker(ctx context.Context, ch <-chan models.Notification) {
	for {
		select {
		case <-ctx.Done():
			return
		case notif := <-ch:
			deliverWebhook(ctx, notif)
		}
	}
}

func deliverWebhook(ctx context.Context, notif models.Notification) {
	payload := models.WebhookEvent{
		Type:      "trackalph.notification",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Data: models.WebhookEventData{
			Message: notif.Message,
			Channel: notif.Channel,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling webhook payload: %v", err)
		return
	}

	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		if ctx.Err() != nil {
			return
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, notif.URL, bytes.NewReader(body))
		if err != nil {
			log.Printf("Error creating webhook request to %s: %v", notif.URL, err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "TrackAlph/1.0")

		resp, err := httpClient.Do(req)
		if err != nil {
			log.Printf("Webhook delivery attempt %d to %s failed: %v", attempt+1, notif.URL, err)
			sleepCtx(ctx, time.Duration(attempt+1)*2*time.Second)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return
		}

		log.Printf("Webhook %s returned %d (attempt %d/%d)", notif.URL, resp.StatusCode, attempt+1, maxRetries)
		sleepCtx(ctx, time.Duration(attempt+1)*2*time.Second)
	}

	log.Printf("Webhook delivery to %s failed after %d attempts", notif.URL, maxRetries)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func init() {
	// Validate that config package is importable (used indirectly for REDIS_URL)
	_ = fmt.Sprintf
}
