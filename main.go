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
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

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

// copyBoth copies in both directions and returns when either side finishes.
func copyBoth(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
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
			log.Printf("publish %s -> %s", c.RemoteAddr(), spec.target)
			copyBoth(c, d)
		}(c)
	}
}

func serveMap(ctx context.Context, srv *tsnet.Server, spec mapSpec, dialTimeout time.Duration) error {
	ln, err := net.Listen("tcp", spec.local)
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
			log.Printf("map %s -> %s", c.RemoteAddr(), spec.target)
			copyBoth(c, d)
		}(c)
	}
}

func main() {
	var publishes, maps multiFlag
	var (
		hostname   = flag.String("hostname", "tsnet-bridge", "node name shown in Headscale")
		controlURL = flag.String("control-url", "https://blackhole.netsnail.net", "Headscale control URL")
		stateDir   = flag.String("state-dir", "./tsnet-bridge-state", "tsnet state directory (unique per node)")
		derpOnly   = flag.Bool("derp-only", false, "force all peer traffic through the DERP relay (VPS)")
		dialTO     = flag.Duration("dial-timeout", 10*time.Second, "timeout for outgoing dials")
	)
	flag.Var(&publishes, "publish", "publish TAILNET_PORT=LOCAL_HOST:PORT (repeatable)")
	flag.Var(&maps, "map", "map LOCAL_ADDR=TAILNET_HOST:PORT (repeatable)")
	flag.Parse()

	authKey := strings.TrimSpace(os.Getenv("TS_AUTHKEY"))
	if authKey == "" {
		log.Fatal("TS_AUTHKEY is required")
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &tsnet.Server{
		Hostname:   *hostname,
		ControlURL: *controlURL,
		AuthKey:    authKey,
		Dir:        *stateDir,
		Logf:       log.Printf,
	}
	defer srv.Close()

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
}
