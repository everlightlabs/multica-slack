FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN mkdir -p /data && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/multica-slack ./cmd/multica-slack

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/multica-slack /multica-slack
COPY --from=build --chown=nonroot:nonroot /data /data
VOLUME ["/data"]
EXPOSE 3020
USER nonroot:nonroot
ENTRYPOINT ["/multica-slack"]
