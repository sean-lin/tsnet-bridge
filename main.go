// Command tsnet-bridge joins a tailnet as a userspace tsnet node and bridges
// TCP in either direction, without root, TUN, or routing-table changes.
//
// Publish mode (local service -> tailnet):
//
//	tsnet-bridge --publish 9000=127.0.0.1:22
//
// Map mode (tailnet service -> local port):
//
//	tsnet-bridge --map 127.0.0.1:2222=100.64.0.19:22
//
// Both modes can be used in the same process and share one tsnet node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
var version = "dev"

// verbose gates per-connection logging. Start/stop lines and errors are always
// logged: a GUI session through the bridge can produce thousands of connection
// pairs, which would otherwise bury the lines that matter.
var verbose bool

// vlogf logs only when -verbose is set.
func vlogf(format string, args ...any) {
	if verbose {
		log.Printf(format, args...)
	}
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

type publishSpec struct {
	port   int
	target string
}

type mapSpec struct {
	local  string
	target string
}

func splitKV(raw string) (string, string, error) {
	left, right, ok := strings.Cut(raw, "=")
	if !ok {
		return "", "", fmt.Errorf("want LEFT=RIGHT, got %q", raw)
	}
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return "", "", fmt.Errorf("empty side in %q", raw)
	}
	return left, right, nil
}

func parsePublish(raw string) (publishSpec, error) {
	left, right, err := splitKV(raw)
	if err != nil {
		return publishSpec{}, err
	}
	left = strings.TrimPrefix(left, ":")
	port, err := strconv.Atoi(left)
	if err != nil || port <= 0 || port > 65535 {
		return publishSpec{}, fmt.Errorf("invalid tailnet port %q", left)
	}
	if _, _, err := net.SplitHostPort(right); err != nil {
		return publishSpec{}, fmt.Errorf("invalid target %q: %w", right, err)
	}
	return publishSpec{port: port, target: right}, nil
}

func parseMap(raw string) (mapSpec, error) {
	left, right, err := splitKV(raw)
	if err != nil {
		return mapSpec{}, err
	}
	if _, _, err := net.SplitHostPort(left); err != nil {
		return mapSpec{}, fmt.Errorf("invalid local address %q: %w", left, err)
	}
	if _, _, err := net.SplitHostPort(right); err != nil {
		return mapSpec{}, fmt.Errorf("invalid tailnet target %q: %w", right, err)
	}
	return mapSpec{local: left, target: right}, nil
}

// closeWriter is the half-close capability both *net.TCPConn and tsnet's own
// TCP conn implement: it flushes what has been written and sends FIN without
// tearing down the other direction.
type closeWriter interface {
	CloseWrite() error
}

// copyBoth copies in both directions and returns once both directions finish.
//
// The first EOF half-closes the opposite side instead of closing the whole
// pair: a client that is done sending must still receive the rest of the
// response (HTTP keep-alive, SSE, WebSocket), which an unconditional close
// would truncate.
func copyBoth(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyOneWay := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
	}
	go copyOneWay(a, b)
	go copyOneWay(b, a)
	wg.Wait()
}

func servePublish(ctx context.Context, srv *tsnet.Server, spec publishSpec, dialTimeout time.Duration) error {
	ln, err := srv.Listen("tcp", fmt.Sprintf(":%d", spec.port))
	if err != nil {
		return fmt.Errorf("listen tailnet :%d: %w", spec.port, err)
	}
	log.Printf("publish tailnet :%d -> %s", spec.port, spec.target)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept tailnet :%d: %w", spec.port, err)
		}
		go func(c net.Conn) {
			defer c.Close()
			d, err := net.DialTimeout("tcp", spec.target, dialTimeout)
			if err != nil {
				log.Printf("publish %s: dial %s: %v", c.RemoteAddr(), spec.target, err)
				return
			}
			defer d.Close()
			vlogf("publish %s -> %s", c.RemoteAddr(), spec.target)
			copyBoth(c, d)
		}(c)
	}
}

// listenReuseAddr listens on the local address with SO_REUSEADDR set explicitly.
//
// Go's own listener already sets it on unix, but only inside the standard
// library's socket setup, which a caller cannot see or rely on across platforms.
// Setting it here makes the intent explicit: a bridge that is restarted while
// previous connections sit in TIME_WAIT must be able to bind again immediately.
func listenReuseAddr(ctx context.Context, addr string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var sockErr error
			if err := c.Control(func(fd uintptr) {
				sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			}); err != nil {
				return err
			}
			return sockErr
		},
	}
	return lc.Listen(ctx, "tcp", addr)
}

func serveMap(ctx context.Context, srv *tsnet.Server, spec mapSpec, dialTimeout time.Duration) error {
	ln, err := listenReuseAddr(ctx, spec.local)
	if err != nil {
		return fmt.Errorf("listen local %s: %w", spec.local, err)
	}
	log.Printf("map local %s -> tailnet %s", spec.local, spec.target)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept local %s: %w", spec.local, err)
		}
		go func(c net.Conn) {
			defer c.Close()
			dctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			d, err := srv.Dial(dctx, "tcp", spec.target)
			if err != nil {
				log.Printf("map %s: dial %s: %v", c.RemoteAddr(), spec.target, err)
				return
			}
			defer d.Close()
			vlogf("map %s -> %s", c.RemoteAddr(), spec.target)
			copyBoth(c, d)
		}(c)
	}
}

// stateHasNode reports whether dir already holds a tsnet node identity. When
// it does, a restart resumes that identity and needs no auth key.
func stateHasNode(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "tailscaled.state"))
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

// lockStateDir takes an exclusive flock on <dir>/lock so two processes can
// never share one tsnet identity. tsnet keeps its node key and profile in the
// state directory and rewrites them in place; two writers corrupt that state,
// and the failure surfaces much later as a node that cannot register. The lock
// is released by the returned function or, on any exit path, by the kernel
// closing the descriptor.
func lockStateDir(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state dir %s: %w", dir, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("state dir %s is in use by another tsnet-bridge instance (remove %s if that is wrong)", dir, path)
		}
		return nil, fmt.Errorf("state dir %s: flock: %w", dir, err)
	}
	// Record the owner for humans; the lock itself is what excludes others.
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// shutdown closes the tsnet node but never blocks past grace. Close tears down
// the WireGuard device, the DERP connections and the IPN backend; any of those
// can stall on network teardown, and a shutdown that hangs is indistinguishable
// from a crash to init systems and supervisors.
func shutdown(closeSrv func(), grace time.Duration) {
	done := make(chan struct{})
	go func() {
		closeSrv()
		close(done)
	}()
	select {
	case <-done:
		log.Printf("closed cleanly")
	case <-time.After(grace):
		log.Printf("close did not finish within %s; exiting anyway", grace)
	}
}

func main() {
	var publishes, maps multiFlag
	var (
		hostname   = flag.String("hostname", "tsnet-bridge", "node name shown in Headscale")
		controlURL = flag.String("control-url", "", "Headscale control URL, required (e.g. https://headscale.example.com)")
		stateDir   = flag.String("state-dir", "./tsnet-bridge-state", "tsnet state directory (unique per node)")
		derpOnly   = flag.Bool("derp-only", false, "force all peer traffic through the DERP relay (VPS)")
		dialTO     = flag.Duration("dial-timeout", 10*time.Second, "timeout for outgoing dials")
		grace      = flag.Duration("shutdown-grace", 5*time.Second, "how long to wait for the tsnet node to close before exiting anyway")
		verboseFlg = flag.Bool("verbose", false, "log every proxied connection (default: only start/stop and errors)")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Var(&publishes, "publish", "publish TAILNET_PORT=LOCAL_HOST:PORT (repeatable)")
	flag.Var(&maps, "map", "map LOCAL_ADDR=TAILNET_HOST:PORT (repeatable)")
	flag.Parse()

	if *showVer {
		fmt.Printf("tsnet-bridge %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}
	verbose = *verboseFlg

	// No default: the control plane is deployment-specific, and a default here
	// would silently point a fresh install at somebody else's Headscale.
	control := strings.TrimRight(strings.TrimSpace(*controlURL), "/")
	if control == "" {
		log.Fatal("--control-url is required, for example: --control-url https://headscale.example.com")
	}
	*controlURL = control

	authKey := strings.TrimSpace(os.Getenv("TS_AUTHKEY"))
	if authKey == "" && !stateHasNode(*stateDir) {
		log.Fatalf("TS_AUTHKEY is required for the first registration (%s holds no node state)", *stateDir)
	}
	if len(publishes) == 0 && len(maps) == 0 {
		log.Fatal("nothing to do: pass at least one --publish or --map")
	}

	// TS_DEBUG_ALWAYS_USE_DERP is read by the tailscale package during init,
	// so it must be set before the process starts. Re-exec once to apply it.
	if *derpOnly && os.Getenv("TS_DEBUG_ALWAYS_USE_DERP") != "1" && os.Getenv("TSNET_BRIDGE_REEXEC") != "1" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("derp-only: resolve executable: %v", err)
		}
		env := append(os.Environ(), "TS_DEBUG_ALWAYS_USE_DERP=1", "TSNET_BRIDGE_REEXEC=1")
		log.Printf("derp-only: re-exec with TS_DEBUG_ALWAYS_USE_DERP=1")
		if err := syscall.Exec(exe, os.Args, env); err != nil {
			log.Fatalf("derp-only: re-exec: %v", err)
		}
	}

	// Taken after the derp-only re-exec: Go opens files with O_CLOEXEC, so a
	// lock acquired before exec would be dropped by the exec itself.
	releaseState, err := lockStateDir(*stateDir)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer releaseState()

	log.Printf("tsnet-bridge %s starting (hostname=%s, derp-only=%t)", version, *hostname, *derpOnly)

	// Two-stage shutdown: the first SIGINT/SIGTERM drains, the second exits at
	// once. signal.NotifyContext is not enough here because it keeps consuming
	// further signals, so an operator who has already waited out a slow drain
	// has no way to force the process down.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-sigCh
		log.Printf("signal received; shutting down (send it again to exit immediately)")
		cancel()
		<-sigCh
		log.Printf("second signal; exiting immediately")
		os.Exit(1)
	}()

	srv := &tsnet.Server{
		Hostname:   *hostname,
		ControlURL: *controlURL,
		AuthKey:    authKey,
		Dir:        *stateDir,
		Logf:       log.Printf,
	}
	// Close exactly once: the normal path closes with a deadline below, while
	// early returns still need the defer.
	var closeOnce sync.Once
	closeSrv := func() {
		closeOnce.Do(func() { _ = srv.Close() })
	}
	defer closeSrv()

	status, err := srv.Up(ctx)
	if err != nil {
		log.Fatalf("tsnet up: %v", err)
	}
	log.Printf("node %q up: %v", status.Self.HostName, status.TailscaleIPs)

	errCh := make(chan error, len(publishes)+len(maps))
	for _, raw := range publishes {
		spec, err := parsePublish(raw)
		if err != nil {
			log.Fatalf("--publish: %v", err)
		}
		go func(spec publishSpec) { errCh <- servePublish(ctx, srv, spec, *dialTO) }(spec)
	}
	for _, raw := range maps {
		spec, err := parseMap(raw)
		if err != nil {
			log.Fatalf("--map: %v", err)
		}
		go func(spec mapSpec) { errCh <- serveMap(ctx, srv, spec, *dialTO) }(spec)
	}

	select {
	case <-ctx.Done():
		log.Printf("shutting down")
	case err := <-errCh:
		if err != nil {
			log.Printf("listener stopped: %v", err)
		}
	}

	shutdown(closeSrv, *grace)
}
