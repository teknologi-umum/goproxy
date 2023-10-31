package internal

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/goproxy/goproxy"
	"github.com/spf13/cobra"
)

// newServerCmd creates a new server command.
func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Start a Go module proxy server",
		Long: strings.TrimSpace(`
Start a Go module proxy server.
`),
	}
	cfg := newServerCmdConfig(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runServerCmd(cmd, args, cfg)
	}
	return cmd
}

// serverCmdConfig is the configuration for server command.
type serverCmdConfig struct {
	address          string
	tlsCertFile      string
	tlsKeyFile       string
	pathPrefix       string
	goBinName        string
	maxDirectFetches int
	proxiedSUMDBs    []string
	cacheDir         string
	tempDir          string
	insecure         bool
	connectTimeout   time.Duration
	fetchTimeout     time.Duration
	shutdownTimeout  time.Duration
}

// newServerCmdConfig creates a new [serverCmdConfig].
func newServerCmdConfig(cmd *cobra.Command) *serverCmdConfig {
	cfg := &serverCmdConfig{}
	fs := cmd.Flags()
	fs.StringVar(&cfg.address, "address", "localhost:8080", "TCP address that the server listens on")
	fs.StringVar(&cfg.tlsCertFile, "tls-cert-file", "", "path to the TLS certificate file")
	fs.StringVar(&cfg.tlsKeyFile, "tls-key-file", "", "path to the TLS key file")
	fs.StringVar(&cfg.pathPrefix, "path-prefix", "", "prefix for all request paths")
	fs.StringVar(&cfg.goBinName, "go-bin-name", "go", "name of the Go binary that is used to execute direct fetches")
	fs.IntVar(&cfg.maxDirectFetches, "max-direct-fetches", 0, "maximum number (0 means no limit) of concurrent direct fetches")
	fs.StringSliceVar(&cfg.proxiedSUMDBs, "proxied-sumdbs", nil, "list of proxied checksum databases")
	fs.StringVar(&cfg.cacheDir, "cache-dir", "caches", "directory that used to cache module files")
	fs.StringVar(&cfg.tempDir, "temp-dir", os.TempDir(), "directory for storing temporary files")
	fs.BoolVar(&cfg.insecure, "insecure", false, "allow insecure TLS connections")
	fs.DurationVar(&cfg.connectTimeout, "connect-timeout", 30*time.Second, "maximum amount of time (0 means no limit) will wait for an outgoing connection to establish")
	fs.DurationVar(&cfg.fetchTimeout, "fetch-timeout", 10*time.Minute, "maximum amount of time (0 means no limit) will wait for a fetch to complete")
	fs.DurationVar(&cfg.shutdownTimeout, "shutdown-timeout", 10*time.Second, "maximum amount of time (0 means no limit) will wait for the server to shutdown")
	return cfg
}

// runServerCmd runs the server command.
func runServerCmd(cmd *cobra.Command, args []string, cfg *serverCmdConfig) error {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: cfg.connectTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: cfg.insecure}
	transport.RegisterProtocol("file", http.NewFileTransport(httpDirFS{}))
	g := &goproxy.Goproxy{
		GoBinName:        cfg.goBinName,
		MaxDirectFetches: cfg.maxDirectFetches,
		ProxiedSUMDBs:    cfg.proxiedSUMDBs,
		Cacher:           goproxy.DirCacher(cfg.cacheDir),
		TempDir:          cfg.tempDir,
		Transport:        transport,
	}

	handler := http.Handler(g)
	if cfg.pathPrefix != "" {
		handler = http.StripPrefix(cfg.pathPrefix, handler)
	}
	if cfg.fetchTimeout > 0 {
		handler = func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				ctx, cancel := context.WithTimeout(req.Context(), cfg.fetchTimeout)
				h.ServeHTTP(rw, req.WithContext(ctx))
				cancel()
			})
		}(handler)
	}

	server := &http.Server{
		Addr:        cfg.address,
		Handler:     handler,
		BaseContext: func(_ net.Listener) context.Context { return cmd.Context() },
	}
	stopCtx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var serverError error
	go func() {
		if cfg.tlsCertFile != "" && cfg.tlsKeyFile != "" {
			serverError = server.ListenAndServeTLS(cfg.tlsCertFile, cfg.tlsKeyFile)
		} else {
			serverError = server.ListenAndServe()
		}
		stop()
	}()
	<-stopCtx.Done()
	if serverError != nil && !errors.Is(serverError, http.ErrServerClosed) {
		return serverError
	}

	shutdownCtx := context.Background()
	if cfg.shutdownTimeout > 0 {
		var cancel context.CancelFunc
		shutdownCtx, cancel = context.WithTimeout(shutdownCtx, cfg.shutdownTimeout)
		defer cancel()
	}
	return server.Shutdown(shutdownCtx)
}

// httpDirFS implements [http.FileSystem] for the local file system.
type httpDirFS struct{}

// Open implements [http.FileSystem].
func (fs httpDirFS) Open(name string) (http.File, error) {
	name = filepath.FromSlash(name)
	if filepath.Separator == '\\' {
		name = name[1:]
		volumeName := filepath.VolumeName(name)
		if volumeName == "" || strings.HasPrefix(volumeName, `\\`) {
			return nil, errors.New("file URL missing drive letter")
		}
	}
	if !filepath.IsAbs(name) {
		return nil, errors.New("path is not absolute")
	}
	return os.Open(name)
}
