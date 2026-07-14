# Control-plane image: carries both paddock-server and paddock-gateway; the
# helm chart runs them as two containers from this one image.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/sqlite is pure Go, so a static build works.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/paddock-server ./cmd/paddock-server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/paddock-gateway ./cmd/paddock-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/paddock-server /paddock-server
COPY --from=build /out/paddock-gateway /paddock-gateway
# No ENTRYPOINT: the chart picks /paddock-server or /paddock-gateway per container.
