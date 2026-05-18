FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.* ./
RUN go mod download
COPY . .
# embed.FS includes migrations — no COPY of migration files needed at runtime
RUN CGO_ENABLED=0 go build -o /bin/aicg-gw ./cmd/aicg-gw

FROM gcr.io/distroless/static-debian12
COPY --from=builder /bin/aicg-gw /bin/aicg-gw
EXPOSE 8443
ENTRYPOINT ["/bin/aicg-gw"]
