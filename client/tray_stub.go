//go:build !windows

package main

import (
	"context"
	"log"
	"time"
)

func runTray(client *SyncClient) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		if err := client.SyncOnce(ctx); err != nil {
			log.Printf("sync failed: %v", err)
		} else {
			log.Printf("sync completed")
		}
		cancel()
		<-ticker.C
	}
}
