# --- build stage ---
FROM golang:1.27-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o linkpulse .

# --- runtime stage ---
FROM alpine:3.20
RUN addgroup -S linkpulse && adduser -S linkpulse -G linkpulse
WORKDIR /app
COPY --from=builder /app/linkpulse .
COPY static ./static
USER linkpulse
EXPOSE 8080
ENTRYPOINT ["./linkpulse"]