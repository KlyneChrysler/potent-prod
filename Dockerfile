# syntax=docker/dockerfile:1.7
FROM golang:1.26-alpine AS build
WORKDIR /src

# cache module downloads
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w" -trimpath -o /out/potent ./cmd/potent

# distroless static — no shell, no package manager, runs as uid 65532 (nonroot)
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/potent /potent
COPY configs/policy.yaml /configs/policy.yaml
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/potent", "-policy", "/configs/policy.yaml"]
