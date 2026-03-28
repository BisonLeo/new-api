#!/bin/bash
GIT_COMMIT=$(git rev-parse --short HEAD) BUILDKIT_PROGRESS=plain DOCKER_BUILDKIT=1 docker compose --progress=plain build --no-cache  new-api
