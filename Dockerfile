FROM golang:1.20-bullseye as builder
WORKDIR /netreap
COPY go.mod go.sum /netreap/
RUN go mod download
COPY . /netreap/
ARG VERSION
RUN go build -ldflags "-s -w -X 'main.Version=$VERSION'"
FROM gcr.io/distroless/base-debian11
WORKDIR /
COPY --from=builder /netreap/netreap /usr/bin/netreap
ENTRYPOINT ["/usr/bin/netreap"]
