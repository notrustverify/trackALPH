package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"trackalph.app/internal/config"
	"trackalph.app/internal/metrics"
	"trackalph.app/internal/models"
	"trackalph.app/internal/stream"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
)

const metricsAddr = ":2112"

var (
	scraperBlocksReceivedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_scraper_blocks_received_total",
		Help: "Total block_notify events received from websocket.",
	})
	scraperBlocksPublishedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_scraper_blocks_published_total",
		Help: "Total blocks published to Redis stream.",
	})
	scraperPublishErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_scraper_publish_errors_total",
		Help: "Total block publish errors.",
	})
	scraperReconnectsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_scraper_ws_reconnects_total",
		Help: "Total websocket reconnect attempts after disconnects.",
	})
	scraperWSConnected = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "trackalph_scraper_ws_connected",
		Help: "Current websocket connection status (1 connected, 0 disconnected).",
	})
)

func main() {
	cfg := config.Load()
	metrics.StartServer(metricsAddr)

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go wsLoop(ctx, cfg, str)

	log.Println("Scraper is running")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down scraper...")
	cancel()
	time.Sleep(500 * time.Millisecond)
	log.Println("Scraper stopped")
}

func wsLoop(ctx context.Context, cfg config.Config, str *stream.Client) {
	backoff := 1 * time.Second
	maxBackoff := 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		connStart := time.Now()
		err := connectAndListen(ctx, cfg, str)
		if err != nil && ctx.Err() == nil {
			scraperReconnectsTotal.Inc()
			log.Printf("WebSocket disconnected: %v, reconnecting in %v", err, backoff)
		}

		if time.Since(connStart) > 30*time.Second {
			backoff = 1 * time.Second
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

func connectAndListen(ctx context.Context, cfg config.Config, str *stream.Client) error {
	u := url.URL{Scheme: "wss", Host: cfg.FullnodeWS, Path: "/events"}
	log.Printf("Connecting to WebSocket: %s", u.String())

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	scraperWSConnected.Set(1)
	defer scraperWSConnected.Set(0)

	log.Println("WebSocket connected")

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
					return
				}
			}
		}
	}()

	go func() {
		<-ctx.Done()
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn.Close()
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read: %w", err)
		}

		var block models.WsBlockNotify
		if err := json.Unmarshal(msg, &block); err != nil {
			continue
		}

		if block.Method != "block_notify" {
			continue
		}
		scraperBlocksReceivedTotal.Inc()

		if err := str.Publish(ctx, stream.BlocksStream, msg); err != nil {
			scraperPublishErrorsTotal.Inc()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("Error publishing block: %v", err)
		} else {
			scraperBlocksPublishedTotal.Inc()
		}
	}
}
