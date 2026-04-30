# MonsterMQ Edge Docker Image

Build the default image from the repository root:

```bash
./docker/build.sh
```

Override the image tag with `IMAGE_NAME=... ./docker/build.sh`.

Run it with persistent SQLite storage:

```bash
docker run --rm \
  --name monstermq-edge \
  -p 1883:1883 \
  -p 1884:1884 \
  -p 8080:8080 \
  -v monstermq-edge-data:/data \
  rocworks/monstermq-edge:latest
```

The image runtime is `scratch`. The Linux broker executable is built in a
temporary Go builder stage and copied into the final image with the default
container config at `/etc/monstermq/config.yaml`.

The bundled config uses SQLite at `/data/monstermq.db` for persistent broker
state and `RetainedStoreType: MEMORY` for retained MQTT messages.
