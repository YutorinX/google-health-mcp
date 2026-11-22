# Build
FROM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
# Let the toolchain follow go.mod's go directive, so a dependency bump that
# raises the minimum Go version doesn't break the image build.
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/google-health-mcp .

# Run: static binary on a minimal, non-root base image.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/google-health-mcp /google-health-mcp
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/google-health-mcp", "serve-http"]
