FROM golang:1.15-alpine AS builder

WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum
# Cache modules
RUN go mod download

# Copy go source
COPY main.go .
COPY rerun_actions.go .

RUN GOARCH=amd64 GOOS=linux go build -o rerun-actions .

# Runnable image.
FROM alpine:latest

COPY --from=builder /workspace/rerun-actions /

ENTRYPOINT ["/rerun-actions"]
