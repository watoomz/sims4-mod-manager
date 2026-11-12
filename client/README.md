# Windows client notes

Build the Windows client with:

```sh
cd client
GOOS=windows GOARCH=amd64 go build -ldflags "-H=windowsgui -X main.ClientVersion=1.1.0" -o Sims4ModManager.exe .
```

Run it with the manager address and (if needed) a local Mods path. The client
also accepts `SIMS4_MODS_DOWNLOAD_LIMIT` or `-download-limit` in bytes per
second; zero means unlimited. The tray reports transfer progress and retries
temporary archive or update download failures.

Self-update expects `GET /api/client/version` to return JSON with `version`,
`download_url`, and a lowercase or uppercase hexadecimal `sha256`. The release
binary is downloaded to a staging file, checked against SHA-256, and replaced
by a detached helper after the running Windows process exits. The helper keeps
a rollback copy until the new client has started.

The Windows client also reads `%APPDATA%\CurseForge\agent\GameInstances\AddonGameInstance.json`
when present. It only reports Sims 4 entries whose installed folder is inside
the configured Mods folder, and sends the relative folder plus the CurseForge
addon/file metadata and module fingerprint. The file is read-only and optional:
missing, oversized, or malformed files are ignored. Absolute paths and the
separate CurseForge `app.db` are not sent or read. The backend checks reported
fingerprints through the existing CurseForge fingerprint API when configured,
then labels matching inventory as CurseForge catalog entries. When that local
file is unavailable, normal sync and inventory audit work as before.
