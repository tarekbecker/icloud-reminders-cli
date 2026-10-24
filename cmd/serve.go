package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/logger"
	"icloud-reminders/internal/mcpserver"
	"icloud-reminders/internal/reminders"
)

var serveCmd = &cobra.Command{
	Use:           "serve",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Run a standalone Reminders MCP server (HTTP or stdio)",
	Long: `Expose Reminders tools over Streamable HTTP at /mcp (default) or stdio.

Authenticate separately with 'reminders auth --data-dir <directory>'. The
server never prompts for credentials or 2FA. Keep HTTP on loopback or a
private network. REMINDERS_MCP_TOKEN optionally requires Bearer authentication.
Use one server per data directory and stop it before administrative CLI writes.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		transport, _ := cmd.Flags().GetString("transport")
		listen, _ := cmd.Flags().GetString("listen")
		timeout, _ := cmd.Flags().GetDuration("request-timeout")
		origins, _ := cmd.Flags().GetStringSlice("allow-origin")
		if transport != "http" && transport != "stdio" {
			return errors.New("transport must be http or stdio")
		}
		if timeout < time.Second || timeout > 15*time.Minute {
			return errors.New("request-timeout must be between 1s and 15m")
		}
		for _, origin := range origins {
			parsed, err := url.Parse(origin)
			if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
				return errors.New("allow-origin must be an exact HTTP(S) origin without path, credentials, query or fragment")
			}
		}
		// CLI backend logging can include titles or upstream error bodies. Keep it
		// disabled even when users pass -v; server logs are deliberately separate.
		logger.SetLevel(-1)
		log := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil))
		backend := reminders.New(cache.ConfigDir, timeout)
		server := mcpserver.New(backend, version, log)
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if transport == "stdio" {
			log.Info("reminders_server_start", "transport", "stdio")
			return server.Run(ctx, &mcp.StdioTransport{})
		}
		token := os.Getenv("REMINDERS_MCP_TOKEN")
		if strings.TrimSpace(token) != token {
			return errors.New("REMINDERS_MCP_TOKEN must not contain surrounding whitespace")
		}
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			return fmt.Errorf("listen failed: %w", err)
		}
		httpServer := &http.Server{
			Handler:           mcpserver.HTTPHandler(server, mcpserver.HTTPOptions{Token: token, AllowedOrigins: origins}),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      timeout + 10*time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    32 << 10,
			BaseContext:       func(net.Listener) context.Context { return ctx },
			ErrorLog:          stdlog.New(io.Discard, "", 0),
		}
		done := make(chan error, 1)
		go func() { done <- httpServer.Serve(listener) }()
		log.Info("reminders_server_start", "transport", "http")
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(shutdown); err != nil {
				_ = httpServer.Close()
			}
			err = <-done
		case err = <-done:
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	},
}

func init() {
	serveCmd.Flags().String("transport", "http", "MCP transport: http or stdio")
	serveCmd.Flags().String("listen", "127.0.0.1:8081", "HTTP listen address; use :8080 inside Docker")
	serveCmd.Flags().Duration("request-timeout", 3*time.Minute, "Tool timeout, including queue wait and iCloud calls")
	serveCmd.Flags().StringSlice("allow-origin", nil, "Explicit HTTP(S) browser origins allowed to access /mcp")
}
