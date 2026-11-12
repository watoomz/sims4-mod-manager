//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"time"

	"github.com/getlantern/systray"
)

func runTray(client *SyncClient) {
	systray.Run(func() {
		systray.SetTitle("Sims 4 Mods")
		systray.SetTooltip("Sims 4 Mod Manager")
		syncNow := systray.AddMenuItem("Sync now", "Apply desired mods and report inventory")
		reaudit := systray.AddMenuItem("Re-audit Mods folder", "Hash package and script files and refresh the server inventory")
		checkUpdate := systray.AddMenuItem("Check for client updates", "Check for a newer client version")
		manager := systray.AddMenuItem("Open manager", "Open the Sims 4 Mod Manager website")
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Exit Sims 4 Mod Manager")
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			sync := func() {
				client.OnTransferProgress = func(progress TransferProgress) {
					if progress.Total > 0 {
						percent := int(progress.Completed * 100 / progress.Total)
						systray.SetTooltip(fmt.Sprintf("Sims 4 Mod Manager: %s %d%%", progress.Kind, percent))
					} else {
						systray.SetTooltip(fmt.Sprintf("Sims 4 Mod Manager: downloading %s (%d bytes)", progress.Kind, progress.Completed))
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				if err := client.SyncOnce(ctx); err != nil {
					log.Printf("sync failed: %v", err)
					systray.SetTooltip("Sims 4 Mod Manager: sync failed")
					return
				}
				log.Printf("sync completed")
				systray.SetTooltip("Sims 4 Mod Manager: synced")
			}
			sync()
			for {
				select {
				case <-syncNow.ClickedCh:
					sync()
				case <-reaudit.ClickedCh:
					client.ForceAudit = true
					sync()
				case <-checkUpdate.ClickedCh:
					if err := client.CheckForUpdate(context.Background()); err != nil {
						log.Printf("client update check failed: %v", err)
						systray.SetTooltip("Sims 4 Mod Manager: update check failed")
					}
				case <-manager.ClickedCh:
					openManager(client.ServerURL)
				case <-quit.ClickedCh:
					systray.Quit()
					return
				case <-ticker.C:
					sync()
				}
			}
		}()
	}, func() {})
}

func openManager(serverURL string) {
	program, args := "xdg-open", []string{serverURL}
	if runtime.GOOS == "windows" {
		program, args = "rundll32", []string{"url.dll,FileProtocolHandler", serverURL}
	} else if runtime.GOOS == "darwin" {
		program = "open"
	}
	if err := exec.Command(program, args...).Start(); err != nil {
		log.Printf("could not open manager: %v", err)
	}
}
