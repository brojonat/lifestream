# The Go service. Pure-Go SQLite (modernc), so CGO stays off.
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-w -s -X main.version=${VERSION}" -o /out/lifestream ./cmd/lifestream

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/lifestream /lifestream
EXPOSE 8080 8081
ENTRYPOINT ["/lifestream"]
CMD ["serve"]
