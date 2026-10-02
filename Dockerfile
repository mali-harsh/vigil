FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN mkdir -p /out/data && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /vigil ./cmd/vigil

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /vigil /vigil
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME /data
EXPOSE 8080
ENV VIGIL_CONFIG=/etc/vigil/vigil.yaml VIGIL_DATA_DIR=/data
HEALTHCHECK NONE
ENTRYPOINT ["/vigil"]
