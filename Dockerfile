FROM --platform=${BUILDPLATFORM} golang:1.20 as builder
WORKDIR /netreap
ENV CGO_ENABLED 0
COPY . /netreap
ARG VERSION
ARG TARGETOS
ARG TARGETARCH
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags "-s -w -X 'main.Version=$VERSION'"

FROM scratch AS bin
COPY --from=builder /netreap/netreap /netreap
ENTRYPOINT ["/netreap"]
