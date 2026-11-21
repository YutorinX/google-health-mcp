# Build
FROM golang:1.27 AS build
# Let the toolchain follow go.mod's go directive, so a dependency bump that
# raises the minimum Go version doesn't break the image build.
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/google-health-mcp .

# Run: static binary on a minimal, non-root base image.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/google-health-mcp /google-health-mcp
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/google-health-mcp", "serve-http"]
