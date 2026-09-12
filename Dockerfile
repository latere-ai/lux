# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

# The developer image: compiles luxd inside the image so `docker build .`
# from a checkout is enough. The release image of spec 017 copies a binary
# the pipeline already built and attested; its runtime stage is this one.

FROM golang:1.27-alpine AS build
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
# luxd forks no binary of its own, so the runtime stage is distroless. It
# dials the providers, the OIDC issuers, and the authorizer and event
# webhooks over TLS, so the base carries the CA roots and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/luxd /usr/local/bin/luxd
EXPOSE 8080 8081
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/luxd"]
# <<< shared runtime base >>>
