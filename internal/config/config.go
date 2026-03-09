package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	TelegramToken string
	FullnodeWS    string
	FullnodeAPI   string
	ExplorerAPI   string
	ExplorerURL   string
	TokenListURL  string
	RedisURL      string
}

func Load() Config {
	if err := godotenv.Load(".env"); err != nil {
		log.Println("No .env file found, reading from environment")
	}

	cfg := Config{
		TelegramToken: os.Getenv("TELEGRAM_TOKEN"),
		FullnodeWS:    getEnvDefault("FULLNODE_WS", "node.mainnet.alephium.org"),
		FullnodeAPI:   getEnvDefault("FULLNODE_API", "https://node.mainnet.alephium.org"),
		ExplorerAPI:   getEnvDefault("EXPLORER_API", "https://backend.mainnet.alephium.org"),
		ExplorerURL:   getEnvDefault("EXPLORER_URL", "https://explorer.alephium.org"),
		TokenListURL:  getEnvDefault("TOKEN_LIST_URL", "https://raw.githubusercontent.com/alephium/token-list/refs/heads/master/tokens/mainnet.json"),
		RedisURL:      getEnvDefault("REDIS_URL", "redis://localhost:6379"),
	}

	return cfg
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
