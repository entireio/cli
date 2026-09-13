package main

import (
	"fmt"
	"log"

	"github.com/entireio/cli/app/api"
	"github.com/entireio/cli/app/config"
)

func main() {
	cfg := config.LoadConfig()
	server := api.NewServer(cfg, nil)

	fmt.Printf("====================================================\n")
	fmt.Printf(" Entire Checkpoint Intelligence Application Server  \n")
	fmt.Printf("====================================================\n")
	fmt.Printf(" Environment: %s\n", cfg.Environment)
	fmt.Printf(" Server URL:  http://%s:%s\n", cfg.ServerHost, cfg.ServerPort)
	fmt.Printf(" REST API:    http://%s:%s/api/health\n", cfg.ServerHost, cfg.ServerPort)
	fmt.Printf(" Dashboard:   http://%s:%s/\n", cfg.ServerHost, cfg.ServerPort)
	fmt.Printf("====================================================\n")

	if err := server.Start(); err != nil {
		log.Fatalf("Server exited with error: %v", err)
	}
}
