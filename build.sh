#!/bin/bash
GIT_COMMIT=$(git rev-parse --short HEAD) \
DOCKER_BUILDKIT=1 \
docker compose build new-api \
  --build-arg GIT_COMMIT="$(git rev-parse --short HEAD)" \
  --progress=plain
