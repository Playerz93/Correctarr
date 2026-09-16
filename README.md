# Correctarr

Correctarr checks that every file your Sonarr and Radarr instances believe they
own is really there, really readable, and really in Plex. It is built for a
debrid / symlink setup (decypharr, rclone, zurg and friends) where the arr
points at a symlink and the symlink points into a FUSE mount.

For each file the arrs report, a sweep checks:

| Check | Issue reported | Automatic fix |
| --- | --- | --- |
| Path missing on disk | `missing file` | Delete the file record in the arr, trigger a search |
| Symlink exists but target is gone | `dead symlink` | Same |
| Resolves but is zero bytes | `empty file` | Same |
| Read-through integrity check fails | `integrity failed` | Same |
| Healthy on disk but Plex has no item for that path | `not in Plex` | Ask Plex to scan that folder |

Findings live in SQLite and move through three states that the dashboard
tallies: **Broken** (found, nothing done), **Need to fix** (a fix was sent,
waiting for the item to come back healthy) and **Fixed** (confirmed healthy on a
later sweep). A fixed item that breaks again reopens as broken.

## Integrity depths

| Depth | What it does | Cost |
| --- | --- | --- |
| off | Disk and Plex checks only | none |
| quick | Reads the first and last 256 KiB, validates the container magic (MKV, MP4, AVI, TS, ...). Catches HTML error pages, zero-filled placeholders and links that die at the tail. | 512 KiB per file |
| standard | quick + `ffprobe`: real container parse, requires a duration and a video stream. Catches truncated or badly assembled files. | a few MiB per file |
| full | standard + stream every byte and compare to the size. | the whole library |

Standard is the default for scheduled sweeps. Full is meant for the
"Integrity" dropdown on the dashboard when you want a one-off deep pass.

## Running on Unraid (Compose Manager)

1. Install the **Compose Manager** plugin from Community Applications.
2. Add a new stack called `correctarr` and paste the contents of
   [`docker-compose.yml`](docker-compose.yml) from this repo.
3. Change the two host paths under `volumes` if yours differ, then **Compose Up**.

The image is built by GitHub Actions and published to
`ghcr.io/playerz93/correctarr:latest` on every push to `main`. While the
repository is private the package is private too: either make the package
public once in GitHub (Packages → correctarr → Package settings → Change
visibility) or run `docker login ghcr.io` on the Unraid console first. You can
also comment out `image:` and uncomment `build:` to build straight from the
repo on Unraid.

The media path must be mounted **exactly as Sonarr, Radarr and Plex see it**,
and must include both the arr root folders (where the symlinks are) and the
debrid mount the symlinks point into. `rslave` propagation keeps the FUSE mount
visible inside the container.

A classic Unraid Docker template is also in `unraid/correctarr.xml`.

Open `http://<host>:8585`. There is no login; keep it on your LAN.

### Environment

| Variable | Default | Purpose |
| --- | --- | --- |
| `CORRECTARR_LISTEN` | `:8585` | Listen address |
| `CORRECTARR_DATA` | `/config` | Where `correctarr.db` is stored |
| `PUID`, `PGID` | `99`, `100` | User and group the process runs as. The config folder is chowned on start. |
| `PLEX_URL`, `PLEX_TOKEN` | | Optional: seed Plex settings on first start. The GUI always wins afterwards. |

## First run

1. Settings → add each Sonarr and Radarr (name, URL, API key) and press
   **Test connection**. The type is detected from the reply. Save each one.
2. Settings → Plex URL and token, **Test connection** lists the movie and show
   sections it can see. Save settings.
3. Dashboard → **Dry run**. The run log shows every finding and what the fix
   would do, without touching anything.
4. When the list looks right, either fix items one by one from the table, or
   arm **Fix all** in Settings and use the button. Turn **Dry run** off in
   Settings when you want fixes to actually be sent.

Safety defaults: dry run is on, auto-fix is off, Fix all is not armed.

## Development

```sh
go build ./cmd/correctarr
./correctarr -listen 127.0.0.1:8585 -data ./data
```

`ffprobe` must be in `PATH` for the standard and full depths; without it the
sweep falls back to quick and says so in the run log. The Docker image
includes it.

## API

Everything the GUI does goes through `/api/...`:

`GET /api/status`, `GET|PUT /api/settings`, `GET|POST /api/arrs`,
`DELETE /api/arrs/{id}`, `POST /api/arrs/test`, `POST /api/plex/test`,
`GET /api/findings?status=&instance=`, `POST /api/findings/{id}/fix`
(`{"action":"auto|research|plexscan"}`), `DELETE /api/findings/{id}`,
`POST /api/findings/fix-all`, `POST /api/findings/clear-fixed`,
`POST /api/sweep` (`{"mode":"manual|dry","depth":""}`), `POST /api/sweep/cancel`,
`GET /api/runs`, `GET /api/runs/{id}`.
