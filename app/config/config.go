package config

import (
	"os"
)

// Config holds runtime configuration settings for the Checkpoint Intelligence application.
type Config struct {
	ServerPort        string
	ServerHost        string
	Environment       string
	GitHubToken       string
	GitHubAPIURL      string
	EntireAPIKey      string
	EntireGraphURL    string
	EntireGraphEnable bool
	DatabricksHost    string
	DatabricksToken   string
}

// LoadConfig reads configuration settings from environment variables with safe fallback defaults.
func LoadConfig() *Config {
	return &Config{
		ServerPort:        getEnvOrDefault("SERVER_PORT", "8080"),
		ServerHost:        getEnvOrDefault("SERVER_HOST", "localhost"),
		Environment:       getEnvOrDefault("ENVIRONMENT", "development"),
		GitHubToken:       os.Getenv("GITHUB_TOKEN"),
		GitHubAPIURL:      getEnvOrDefault("GITHUB_API_URL", "https://api.github.com"),
		EntireAPIKey:      os.Getenv("ENTIRE_API_KEY"),
		EntireGraphURL:    getEnvOrDefault("ENTIRE_GRAPH_ENDPOINT", "http://localhost:9090"),
		EntireGraphEnable: getEnvOrDefault("ENTIRE_GRAPH_ENABLED", "true") == "true",
		DatabricksHost:    os.Getenv("DATABRICKS_HOST"),
		DatabricksToken:   os.Getenv("DATABRICKS_TOKEN"),
	}
}

func getEnvOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
