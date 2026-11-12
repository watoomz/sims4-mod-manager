# Sims 4 Mod Manager

A self-hosted catalog and desired-state manager for Sims 4 mods. The Go backend
serves an embedded web interface and HTTP API, stores its catalog in SQLite,
and keeps uploaded mod archives on disk. A separate Windows tray client syncs
desired mods and inventory with the backend over HTTP.

## Quickstart

Requirements: Docker Engine with the Compose plugin.

```sh
git clone <repository-url>
cd sims4-mod-manager-oss
cp .env.example .env
```

Edit `.env` if you want to set a CurseForge key, client release version, host
port, or persistent data directories. Then start the service:

```sh
docker compose up -d
```

Open <http://localhost:8080>. The backend health check is available at
`/healthz`; the API also exposes `/api/status`, `/api/mods`, `/api/sources`, and
`/api/search?q=...`. The home page and API run together in the backend
container.

## Windows client

The Windows tray client is a separately built and downloaded native `.exe`; it
does not run in Compose. Build it from `client/` with Go, following
[client/README.md](client/README.md). Run
the client on each Windows PC and point it at the machine running Compose, for
example:

```powershell
Sims4ModManager.exe -server http://<host-running-compose>:8080
```

You can also set `SIMS4_MODS_MANAGER_URL`. The client manages the local Sims 4
Mods folder and syncs with the server; see [client/README.md](client/README.md)
for its options and behavior.

## Optional CurseForge features

CurseForge integration is disabled when `CURSEFORGE_API_KEY` is blank. To
enable it, obtain a key from [CurseForge](https://console.curseforge.com/) and
put it in `.env`, then recreate the backend with `docker compose up -d`. The
key enables the backend's CurseForge catalog, file, download, and fingerprint
features; the rest of the app works without it.

## Data and backups

The database and archive directories are Docker named volumes (`db_data`,
`archive_data`), not host-relative paths -- this is deliberate: the backend
container runs as a non-root user, and a host bind-mount's ownership
usually doesn't match that user, which breaks the database on a fresh
clone. Named volumes avoid that entirely and survive `docker compose down`
and `docker compose up`.

To back them up:

```sh
docker run --rm -v sims4-mod-manager-oss_db_data:/data -v "$PWD":/backup alpine \
  sh -c 'cd /data && tar czf /backup/db-backup.tar.gz .'
docker run --rm -v sims4-mod-manager-oss_archive_data:/archive -v "$PWD":/backup alpine \
  sh -c 'cd /archive && tar czf /backup/archive-backup.tar.gz .'
```

(The volume name prefix matches your project/folder name; run `docker volume
ls` to confirm it if you renamed the cloned directory.) If you'd rather use a
host directory directly, edit `docker-compose.yml`'s `volumes:` entries to a
host path, but make sure that host directory is writable by UID 100 / GID
101 (the container's non-root user) first.

## Development and contributions

The backend is in `backend/` and the Windows client is in `client/`. Contributions
are welcome; please include a clear description of changes and run the relevant
Go test suite. See [client/README.md](client/README.md) for client build and
protocol notes. This project is licensed under the MIT License; see
[LICENSE](LICENSE).
