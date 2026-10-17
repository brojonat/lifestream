#!/bin/bash
# Build and push both images tagged with the git commit, then apply the
# manifests. Needs DOCKER_REPO (e.g. registry.example.com/me) and
# LIFESTREAM_HOST (the public hostname). Set them in .env; mise loads it.
set -euo pipefail
: "${DOCKER_REPO:?set DOCKER_REPO}" "${LIFESTREAM_HOST:?set LIFESTREAM_HOST}"
cd "$(dirname "$0")/.."
if [ -n "$(git status --porcelain)" ]; then
  echo "deploy: working tree is dirty; commit first so the tag means something" >&2
  exit 1
fi
TAG=$(git rev-parse --short HEAD)

docker build --build-arg VERSION="$TAG" -t "$DOCKER_REPO/lifestream:$TAG" .
docker build -t "$DOCKER_REPO/lifestream-agent:$TAG" agent
docker push "$DOCKER_REPO/lifestream:$TAG"
docker push "$DOCKER_REPO/lifestream-agent:$TAG"

kustomize build deploy/k8s |
  sed -e "s;{{DOCKER_REPO}};$DOCKER_REPO;g" -e "s;{{TAG}};$TAG;g" -e "s;{{HOST}};$LIFESTREAM_HOST;g" |
  kubectl apply -f -
kubectl -n lifestream rollout status deploy/lifestream
