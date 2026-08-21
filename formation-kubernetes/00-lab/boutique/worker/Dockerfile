# syntax=docker/dockerfile:1.10
# ---------------------------------------------------------------------------
# boutique-worker — image de production
#
#   docker build -t registry.lab.techledger.io/boutique/worker:1.0.0 \
#     --build-arg VERSION=1.0.0 \
#     --build-arg COMMIT=$(git rev-parse --short HEAD) .
# ---------------------------------------------------------------------------

FROM golang:1.26.5-alpine3.24 AS build

ARG VERSION=dev
ARG COMMIT=inconnu

WORKDIR /src

# Dépendances d'abord : couche stable, réutilisée tant que go.mod/go.sum
# ne changent pas.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# CGO_ENABLED=0  → binaire statique, exécutable sur distroless/static
# -trimpath      → build reproductible, pas de chemin du builder dans le binaire
# -s -w          → sans symboles ni debug, ~30 % plus petit
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/api .

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="boutique-worker" \
      org.opencontainers.image.description="Worker asynchrone de l Application Boutique" \
      org.opencontainers.image.source="https://gitlab.techledger.io/boutique/worker" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="TechLedger"

COPY --from=build /out/api /api

# UID numérique obligatoire pour runAsNonRoot (module 08).
USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/api"]
