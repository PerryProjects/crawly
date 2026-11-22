# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/siteinfo ./cmd/siteinfo

# Static binary, no shell, no package manager, non-root (uid 65532).
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/siteinfo /siteinfo
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/siteinfo"]
CMD ["serve"]
