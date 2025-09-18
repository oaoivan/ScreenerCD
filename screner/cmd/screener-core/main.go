package main

import (
	"github.com/yourusername/screner/internal/config"
	"github.com/yourusername/screner/internal/exchange"
	"github.com/yourusername/screner/internal/processor"
	"github.com/yourusername/screner/internal/redisclient"
	"github.com/yourusername/screner/internal/util"
)

func main() {
	util.Infof("starting Screener Core")
	// Load configuration
	cfg, err := config.LoadConfig("configs/screener-core.yaml")
	if err != nil {
		util.Fatalf("Error loading config: %v", err)
	}

	// Initialize Redis client
	redisClient := redisclient.NewClient(cfg.Redis)

	// Initialize exchange connectors
	connectors := exchange.NewConnectors(cfg.Exchanges)

	// Start the data processing logic
	proc := processor.NewProcessor(redisClient, connectors)
	if err := proc.Start(); err != nil {
		util.Fatalf("Error starting processor: %v", err)
	}

	util.Infof("Screener Core service is running...")
}
