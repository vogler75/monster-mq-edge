# MonsterMQ Edge Docker Image

Build a local image (native platform) and save it as a tar file:

```bash
./docker/build.sh -n
```

Build multi-arch (`linux/amd64,linux/arm64,linux/arm/v7`) and publish to Docker Hub
(`rocworks/monstermq-edge:<version>` and `:latest`):

```bash
./docker/build.sh -y
```

Without `-n` or `-y`, the script asks. The version comes from `version.txt` at the
repository root and is injected into the binary at build time via Go ldflags.
Override the image repository with `IMAGE_REPO=... ./docker/build.sh`. Override
the platform list with `PLATFORMS=... ./docker/build.sh -y`.

To bump the patch version, tag the release, and write release notes, run from the
repository root:

```bash
./release.sh
```

## Running the image

Run with persistent SQLite storage on the host:

```bash
mkdir -p ./monstermq-data

docker run --rm -d \
  --name monstermq-edge \
  -p 1883:1883 \
  -p 1884:1884 \
  -p 4000:4000 \
  -v "$(pwd)/monstermq-data:/cfg-data" \
  rocworks/monstermq-edge:latest
```

The default config stores the database at `/cfg-data/monstermq.db`, so the host
directory will contain `monstermq.db` (plus `-shm` / `-wal` files at runtime).

To override the config, drop a `config.yaml` into the mounted `/cfg-data`
directory — the entrypoint uses `/cfg-data/config.yaml` if present, otherwise
it falls back to the bundled default at `/etc/monstermq/config.yaml`:

```bash
cp my-config.yaml ./monstermq-data/config.yaml

docker run --rm -d \
  --name monstermq-edge \
  -p 1883:1883 \
  -p 1884:1884 \
  -p 4000:4000 \
  -v "$(pwd)/monstermq-data:/cfg-data" \
  rocworks/monstermq-edge:latest
```

Or mount the config from a separate location:

```bash
docker run --rm -d \
  --name monstermq-edge \
  -p 1883:1883 \
  -p 1884:1884 \
  -p 4000:4000 \
  -v "$(pwd)/monstermq-data:/cfg-data" \
  -v "$(pwd)/my-config.yaml:/cfg-data/config.yaml:ro" \
  rocworks/monstermq-edge:latest
```

### Ports

| Port | Purpose                                        |
|------|------------------------------------------------|
| 1883 | MQTT TCP                                       |
| 1884 | MQTT WebSocket                                 |
| 4000 | GraphQL management API                         |
| 8883 | MQTT TLS (only when enabled in config)         |
| 8884 | MQTT WSS (only when enabled in config)         |

### Image internals

The runtime image is based on `alpine:3`. The broker is a single static Go
binary built in a multi-stage Go builder and copied into the final image, with
the default container config at `/etc/monstermq/config.yaml`. The bundled
config uses SQLite at `/cfg-data/monstermq.db` for persistent broker state and
`RetainedStoreType: MEMORY` for retained MQTT messages.
