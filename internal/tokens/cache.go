package tokens

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sync"
	"time"

	"trackalph.app/internal/models"
)

type Token struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
}

type tokenListResponse struct {
	NetworkID int     `json:"networkId"`
	Tokens    []Token `json:"tokens"`
}

type Cache struct {
	mu     sync.RWMutex
	tokens map[string]Token
	url    string
}

func NewCache(url string) *Cache {
	return &Cache{
		tokens: make(map[string]Token),
		url:    url,
	}
}

func (c *Cache) Start(ctx context.Context) error {
	if err := c.refresh(); err != nil {
		return fmt.Errorf("initial token list fetch: %w", err)
	}
	log.Printf("Loaded %d tokens", c.Len())

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.refresh(); err != nil {
					log.Printf("Error refreshing token list: %v", err)
				} else {
					log.Printf("Refreshed token list, %d tokens", c.Len())
				}
			}
		}
	}()

	return nil
}

func (c *Cache) refresh() error {
	resp, err := http.Get(c.url)
	if err != nil {
		return fmt.Errorf("fetch token list: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token list returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read token list body: %w", err)
	}

	var list tokenListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("parse token list: %w", err)
	}

	m := make(map[string]Token, len(list.Tokens))
	for _, t := range list.Tokens {
		m[t.ID] = t
	}

	c.mu.Lock()
	c.tokens = m
	c.mu.Unlock()

	return nil
}

func (c *Cache) Lookup(id string) (Token, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tokens[id]
	return t, ok
}

func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.tokens)
}

const alphID = "0000000000000000000000000000000000000000000000000000000000000000"

// HumanizeAmount converts a raw amount string to a human-readable float given a token ID.
func (c *Cache) HumanizeAmount(tokenID string, rawAmount string) (float64, string, bool) {
	if tokenID == alphID || tokenID == "" {
		return 0, "ALPH", false
	}

	tok, found := c.Lookup(tokenID)
	if !found {
		return 0, "UNKNOWN", false
	}

	amount := models.ParseFloat(rawAmount)
	if tok.Decimals > 0 {
		amount = amount / math.Pow(10, float64(tok.Decimals))
	}

	return amount, tok.Symbol, true
}
