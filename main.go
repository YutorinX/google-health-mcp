// Command google-health-mcp is an MCP server that gives AI agents (any MCP client) read-only
// access to your Google Health (Fitbit / Pixel Watch) data.
//
//	google-health-mcp login    # one-time browser consent, saves a token
//	google-health-mcp status   # check the saved login
//	google-health-mcp logout   # delete the saved token
//	google-health-mcp [serve]  # run the MCP server on stdio
//	google-health-mcp serve-http # hosted mode (Cloud Run): Streamable HTTP + OAuth
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
	_ "time/tzdata" // the container image has no zoneinfo

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/shmsi/google-health-mcp/internal/auth"
	"github.com/shmsi/google-health-mcp/internal/health"
	"github.com/shmsi/google-health-mcp/internal/remote"
	"github.com/shmsi/google-health-mcp/internal/tools"
)

const version = "0.1.0"

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr) // stdout belongs to the MCP protocol

	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	extra := fs.String("extra-scopes", os.Getenv("GOOGLE_HEALTH_EXTRA_SCOPES"),
		"comma-separated extra read scopes to request at login, e.g. nutrition.readonly,ecg.readonly,irn.readonly,location.readonly")
	tz := fs.String("tz", os.Getenv("GOOGLE_HEALTH_TZ"), "IANA time zone for 'today' and local times (default: system zone)")
	noBrowser := fs.Bool("no-browser", false, "login: print the URL instead of opening a browser")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, cmd, *extra, *tz, !*noBrowser); err != nil {
		log.Fatalf("google-health-mcp: %v", err)
	}
}

func run(ctx context.Context, cmd, extra, tz string, openBrowser bool) error {
	dir, err := auth.Dir()
	if err != nil {
		return err
	}
	scopes := append([]string{}, auth.DefaultScopes...)
	for _, s := range strings.Split(extra, ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopes = append(scopes, s)
		}
	}
	tokenPath := auth.TokenPath(dir)

	switch cmd {
	case "login":
		cfg, err := auth.LoadConfig(dir, scopes)
		if err != nil {
			return err
		}
		_, granted, err := auth.Login(ctx, cfg, tokenPath, openBrowser, os.Stderr)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "\nSaved login to %s\n", tokenPath)
		if missing := auth.MissingScopes(cfg.Scopes, granted); len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "Note: these permissions were not granted, so related data will be unavailable: %s\n", strings.Join(missing, ", "))
		}
		return status(ctx, cfg, tokenPath)

	case "status":
		cfg, err := auth.LoadConfig(dir, scopes)
		if err != nil {
			return err
		}
		return status(ctx, cfg, tokenPath)

	case "logout":
		if err := os.Remove(tokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fmt.Fprintln(os.Stderr, "Removed saved token. To fully revoke access, visit https://myaccount.google.com/permissions")
		return nil

	case "serve":
		loc, err := loadLocation(tz)
		if err != nil {
			return err
		}
		cfg, err := auth.LoadConfig(dir, scopes)
		if err != nil {
			// Start anyway so the client shows a useful error per tool call.
			log.Printf("warning: %v", err)
			cfg = &oauth2.Config{}
		}
		client := health.NewClient(oauth2.NewClient(ctx, auth.NewFileTokenSource(cfg, tokenPath)), os.Getenv("GOOGLE_HEALTH_API_BASE"))

		s := tools.NewServer(tools.Deps{Client: client, Location: loc}, version, nil)
		return s.Run(ctx, &mcp.StdioTransport{})

	case "serve-http":
		return serveHTTP(ctx, scopes, tz)

	case "version", "-version", "--version":
		fmt.Println(version)
		return nil
	}
	return fmt.Errorf("unknown command %q (use login, status, logout, serve or serve-http)", cmd)
}

func status(ctx context.Context, cfg *oauth2.Config, tokenPath string) error {
	tok, granted, err := auth.LoadToken(tokenPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Token file: %s\n", tokenPath)
	if !tok.Expiry.IsZero() {
		fmt.Fprintf(os.Stderr, "Access token expires: %s (refreshed automatically)\n", tok.Expiry.Local().Format(time.RFC1123))
	}
	if granted != "" {
		fmt.Fprintf(os.Stderr, "Granted scopes: %s\n", strings.ReplaceAll(granted, auth.ScopePrefix, ""))
	}
	client := health.NewClient(oauth2.NewClient(ctx, auth.NewFileTokenSource(cfg, tokenPath)), os.Getenv("GOOGLE_HEALTH_API_BASE"))
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	id, err := client.Get(cctx, "identity")
	if err != nil {
		return fmt.Errorf("API check failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "API check OK: %s\n", id)
	return nil
}

func loadLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("invalid -tz %q: %w", name, err)
	}
	return loc, nil
}

// serveHTTP runs the hosted server. Configuration comes from the
// environment (see README "Hosting on Cloud Run").
func serveHTTP(ctx context.Context, scopes []string, tz string) error {
	loc, err := loadLocation(tz)
	if err != nil {
		return err
	}
	key, err := decodeKey(os.Getenv("MCP_SIGNING_KEY"))
	if err != nil {
		return err
	}
	// ALLOWED_EMAILS="*" opens the server to any Google account (public
	// mode). It must be explicit so a missing value never opens it up.
	var emails []string
	anyAccount := strings.TrimSpace(os.Getenv("ALLOWED_EMAILS")) == "*"
	for _, e := range strings.Split(os.Getenv("ALLOWED_EMAILS"), ",") {
		if e = strings.TrimSpace(e); e != "" && e != "*" {
			emails = append(emails, e)
		}
	}
	var hosts []string
	for _, h := range strings.Split(os.Getenv("ALLOWED_REDIRECT_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	h, err := remote.NewHandler(remote.Config{
		PublicURL:          os.Getenv("PUBLIC_URL"),
		GoogleClientID:     os.Getenv("GOOGLE_OAUTH_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
		AllowedEmails:      emails,
		AllowAnyAccount:    anyAccount,
		Key:                key,
		HealthScopes:       auth.FullScopes(scopes),
		RedirectHosts:      hosts,
		Location:           loc,
		Version:            version,
		HealthBaseURL:      os.Getenv("GOOGLE_HEALTH_API_BASE"),
	})
	if err != nil {
		return err
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{Addr: ":" + port, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("google-health-mcp %s listening on :%s (public URL %s)", version, port, os.Getenv("PUBLIC_URL"))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func decodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("MCP_SIGNING_KEY is not set (generate one with: openssl rand -base64 32)")
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) >= 32 {
			return b, nil
		}
	}
	return nil, errors.New("MCP_SIGNING_KEY must be base64 of at least 32 random bytes")
}
