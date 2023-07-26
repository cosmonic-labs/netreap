FROM --platform=${BUILDPLATFORM} golang:1.20.5 as builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
ARG VERSION
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags "-s -w -X 'main.Version=$VERSION'" -o /netreap
FROM scratch AS bin
WORKDIR /
COPY --from=builder /netreap /netreap
ENTRYPOINT ["/netreap"]
