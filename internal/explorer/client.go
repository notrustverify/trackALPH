package explorer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"trackalph.app/internal/models"

	"github.com/hashicorp/go-retryablehttp"
)

type Client struct {
	baseURL string
	client  *http.Client
}

func New(baseURL string) *Client {
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 3
	retryClient.Logger = nil
	retryClient.RetryWaitMin = 1 * time.Second
	retryClient.RetryWaitMax = 5 * time.Second

	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  retryClient.StandardClient(),
	}
}

// FetchTransaction polls the explorer until the tx is accepted or the context is cancelled.
func (c *Client) FetchTransaction(ctx context.Context, txID string) (*models.Transaction, error) {
	reqURL := fmt.Sprintf("%s/transactions/%s", c.baseURL, txID)

	backoff := 2 * time.Second
	maxBackoff := 30 * time.Second
	maxAttempts := 60

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		resp, err := c.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			log.Printf("Explorer request error for tx %s: %v", txID, err)
			if !sleepCtx(ctx, backoff) {
				return nil, ctx.Err()
			}
			backoff = minDuration(backoff*2, maxBackoff)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			if !sleepCtx(ctx, backoff) {
				return nil, ctx.Err()
			}
			backoff = minDuration(backoff*2, maxBackoff)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			log.Printf("Explorer returned %d for tx %s", resp.StatusCode, txID)
			if !sleepCtx(ctx, backoff) {
				return nil, ctx.Err()
			}
			backoff = minDuration(backoff*2, maxBackoff)
			continue
		}

		var tx models.Transaction
		if err := json.Unmarshal(body, &tx); err != nil {
			return nil, fmt.Errorf("unmarshal tx %s: %w", txID, err)
		}

		if strings.EqualFold(tx.Type, "accepted") {
			return &tx, nil
		}

		if !sleepCtx(ctx, backoff) {
			return nil, ctx.Err()
		}
		backoff = minDuration(backoff*2, maxBackoff)
	}

	return nil, fmt.Errorf("tx %s not accepted after max attempts", txID)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
