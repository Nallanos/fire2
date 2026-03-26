package app

import "os"

type Config struct {
	Port        string
	DatabaseURL string
}

func ConfigFromEnv() Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgresql://temporal:temporal@localhost/temporal"
	}

	return Config{Port: port, DatabaseURL: databaseURL}
}
