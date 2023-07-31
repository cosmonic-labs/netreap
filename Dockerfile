FROM golang:1.20-bullseye as builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
ARG VERSION
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X 'main.Version=$VERSION'" -o /netreap
FROM gcr.io/distroless/base-debian11
WORKDIR /
COPY --from=builder /netreap /netreap
ENTRYPOINT ["/netreap"]
