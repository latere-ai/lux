# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

# The developer image: compiles luxd inside the image so `docker build .`
# from a checkout is enough. The release image of spec 017,
# Dockerfile.release, copies a binary the pipeline already built and
# attested; its runtime stage is this one, byte for byte between the two
# markers, which TestRuntimeStagesMatch compares.

FROM golang:1.27-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-X latere.ai/x/lux/internal/version.Version=${VERSION} -X latere.ai/x/lux/internal/version.Commit=${COMMIT} -X latere.ai/x/lux/internal/version.Date=${DATE}" \
      -o /out/luxd ./cmd/luxd

# >>> shared runtime base <<<
# luxd forks no binary of its own, so the runtime stage is distroless,
# pinned by digest. It dials the providers, the OIDC issuers, and the
# authorizer and event webhooks over TLS, so the base carries the CA roots
# and nothing else, and runs as the image's non-root user with both
# listeners' ports exposed.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
EXPOSE 8080 8081
USER nonroot:nonroot
# <<< shared runtime base >>>
COPY --from=build /out/luxd /usr/local/bin/luxd
ENTRYPOINT ["/usr/local/bin/luxd"]
