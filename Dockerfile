FROM golang:1.20 as builder
WORKDIR /netreap
COPY . /netreap
ARG VERSION
RUN go build -ldflags "-s -w -X 'main.Version=$VERSION'"

FROM gcr.io/distroless/base-debian11
COPY --from=builder /netreap/netreap /usr/bin/netreap
ENTRYPOINT ["/usr/bin/netreap"]
