FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build -o /out/potent ./cmd/potent

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/potent /potent
COPY configs/policy.yaml /configs/policy.yaml
EXPOSE 8080
ENTRYPOINT ["/potent", "-policy", "/configs/policy.yaml"]
