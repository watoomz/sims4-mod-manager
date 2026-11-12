package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ClientVersion is overridden by release builds with -ldflags.
var ClientVersion = "0.1.0"

type clientConfig struct {
	ServerURL string `json:"server_url"`
	ClientID  string `json:"client_id"`
	ModsDir   string `json:"mods_dir"`
}
type registerResponse struct {
	ID string `json:"id"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--s4mm-replace" {
		if err := runReplacementHelper(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--s4mm-replace-cleanup" {
		if err := cleanupReplacementBackup(os.Args[2:]); err != nil {
			log.Printf("update backup cleanup failed: %v", err)
		}
		return
	}
	configFile := defaultConfigPath()
	config := loadConfig(configFile)
	server := flag.String("server", firstNonEmpty(os.Getenv("SIMS4_MODS_MANAGER_URL"), config.ServerURL), "manager base URL")
	clientID := flag.String("client-id", firstNonEmpty(os.Getenv("SIMS4_MODS_CLIENT_ID"), config.ClientID), "registered client ID")
	modsDir := flag.String("mods-dir", firstNonEmpty(os.Getenv("SIMS4_MODS_DIR"), config.ModsDir, DetectModsDir()), "Sims 4 Mods folder")
	configFlag := flag.String("config", configFile, "client configuration file")
	downloadLimit := flag.Int64("download-limit", envInt64("SIMS4_MODS_DOWNLOAD_LIMIT", 0), "maximum download rate in bytes per second (0 is unlimited)")
	once := flag.Bool("once", false, "sync once and exit")
	reaudit := flag.Bool("reaudit", false, "rescan the Mods folder and replace the server inventory snapshot")
	flag.Parse()
	if strings.TrimSpace(*server) == "" {
		log.Fatal("set -server or SIMS4_MODS_MANAGER_URL")
	}
	if strings.TrimSpace(*modsDir) == "" {
		log.Fatal("Sims 4 Mods folder not found; set -mods-dir")
	}
	if *clientID == "" {
		id, err := register(*server, *modsDir)
		if err != nil {
			log.Fatal(err)
		}
		*clientID = id
	}
	if err := saveConfig(*configFlag, clientConfig{ServerURL: *server, ClientID: *clientID, ModsDir: *modsDir}); err != nil {
		log.Printf("could not save client configuration: %v", err)
	}
	app := &SyncClient{ServerURL: *server, ClientID: *clientID, ModsDir: *modsDir, ForceAudit: *reaudit, HTTPClient: &http.Client{Timeout: 10 * time.Minute}, DownloadLimitBytesPerSecond: *downloadLimit}
	if *once {
		if err := app.SyncOnce(context.Background()); err != nil {
			log.Fatal(err)
		}
		return
	}
	runTray(app)
}

func envInt64(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func register(server, modsDir string) (string, error) {
	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "Sims 4 PC"
	}
	body, _ := json.Marshal(map[string]string{"name": name, "local_path": modsDir})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(server, "/")+"/api/clients", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("client registration returned HTTP %d", resp.StatusCode)
	}
	var result registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.ID == "" {
		return "", fmt.Errorf("server returned an empty client ID")
	}
	return result.ID, nil
}

func defaultConfigPath() string {
	root, err := os.UserConfigDir()
	if err != nil {
		root, _ = os.UserHomeDir()
	}
	return filepath.Join(root, "Sims4ModManager", "config.json")
}
func loadConfig(file string) clientConfig {
	data, err := os.ReadFile(file)
	if err != nil {
		return clientConfig{}
	}
	var cfg clientConfig
	if json.Unmarshal(data, &cfg) != nil {
		return clientConfig{}
	}
	return cfg
}
func saveConfig(file string, cfg clientConfig) error {
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, data, 0600)
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
