package main

import (
	"log"
	"time"
)

func main() {
	log.Printf("keepalive started")
	for {
		time.Sleep(1 * time.Hour)
	}
}
