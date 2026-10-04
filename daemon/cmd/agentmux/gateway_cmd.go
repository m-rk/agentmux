package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/gateway"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/ops"
)

const (
	defaultGatewayBin = "/usr/local/bin/agentmux"
	// loopbackTestPrincipal is the principal of every request in
	// -insecure-test-grants mode.
	loopbackTestPrincipal = "loopback-test"
)

const gatewayUsage = `usage: agentmux gateway run -capability NAME [-listen ADDR] [-socket PATH] [-tailscale PATH]
                            [-send-rate N/min] [-send-burst N] [-rate N/min] [-burst N]
                            [-insecure-test-grants FILE]
       agentmux gateway install -capability NAME [-listen ADDR] [-bin PATH] [-run-user USER] [-print]`

// runGatewayCmd is `agentmux gateway`: the per-host HTTP service that lets an
// orchestrator on another tailnet host list, read and message this host's
// sessions. See docs/gateway.md.
func runGatewayCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, gatewayUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "run":
		runGatewayRun(args[1:])
	case "install":
		runGatewayInstall(args[1:])
	default:
		fmt.Fprintln(os.Stderr, gatewayUsage)
		os.Exit(2)
	}
}

func runGatewayRun(args []string) {
	fs := flag.NewFlagSet("gateway run", flag.ExitOnError)
	listen := fs.String("listen", "", "address to listen on (default: this host's tailnet IPv4 on port "+strconv.Itoa(gatewayapi.DefaultPort)+"); must be a tailnet address")
	capability := fs.String("capability", "", "app capability name the tailnet policy grants, e.g. example.com/cap/agentmux-gateway (required unless -insecure-test-grants)")
	socket := fs.String("socket", daemoninstall.SocketPath(), "Unix socket agentmuxd is listening on")
	tailscale := fs.String("tailscale", "tailscale", "tailscale CLI binary")
	sendRate := fs.String("send-rate", "30/min", "sustained send rate per caller, N/min (0 turns the limit off)")
	sendBurst := fs.Int("send-burst", gateway.DefaultSendLimit.Burst, "send burst per caller")
	rate := fs.String("rate", "300/min", "sustained rate per caller for every other operation, N/min (0 turns the limit off)")
	burst := fs.Int("burst", gateway.DefaultOtherLimit.Burst, "burst per caller for every other operation")
	testGrants := fs.String("insecure-test-grants", "", "FILE of grants (JSON array); serve loopback only and treat every request as principal "+loopbackTestPrincipal+" with these grants, skipping tailscale whois. For trying the gateway without a tailnet policy; never use in service")
	fs.Parse(args)

	if *capability == "" && *testGrants == "" {
		log.Fatal("gateway run: -capability is required (the app capability name in your tailnet policy grant)")
	}
	sendLimit, err := parseRate(*sendRate, *sendBurst)
	if err != nil {
		log.Fatalf("gateway run: -send-rate: %v", err)
	}
	otherLimit, err := parseRate(*rate, *burst)
	if err != nil {
		log.Fatalf("gateway run: -rate: %v", err)
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	whois := gateway.TailscaleWhois(*tailscale, *capability)
	addr := *listen
	if *testGrants != "" {
		grants, err := readTestGrants(*testGrants)
		if err != nil {
			log.Fatalf("gateway run: %v", err)
		}
		if addr == "" {
			addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(gatewayapi.DefaultPort))
		}
		whois = func(context.Context, string) (gateway.Identity, error) {
			return gateway.Identity{Node: loopbackTestPrincipal, Grants: grants}, nil
		}
		logger.Printf("WARNING: -insecure-test-grants: every request on %s is trusted as %q with the grants in %s. No tailnet identity is checked. For testing only.", addr, loopbackTestPrincipal, *testGrants)
	} else if addr == "" {
		ip, err := tailnetIPv4(*tailscale)
		if err != nil {
			log.Fatalf("gateway run: finding this host's tailnet address (pass -listen to set it): %v", err)
		}
		addr = net.JoinHostPort(ip, strconv.Itoa(gatewayapi.DefaultPort))
	}
	if err := checkListenAddr(addr, *testGrants != ""); err != nil {
		log.Fatalf("gateway run: %v", err)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("gateway run: %v", err)
	}
	handler := gateway.New(gateway.Config{
		Backend:    gateway.LocalBackend{Env: ops.Env{SocketPath: *socket}},
		Whois:      whois,
		Logger:     logger,
		SendLimit:  &sendLimit,
		OtherLimit: &otherLimit,
	})
	// No write timeout: a send may wait out a busy session for minutes.
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if *testGrants != "" {
		logger.Printf("gateway: listening on %s (test grants)", ln.Addr())
	} else {
		logger.Printf("gateway: listening on %s (capability %s)", ln.Addr(), *capability)
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("gateway run: %v", err)
	}
}

// parseRate reads "N/min" or "N" (per minute) into a Limit. 0 turns it off.
func parseRate(s string, burst int) (gateway.Limit, error) {
	n, _ := strings.CutSuffix(strings.TrimSpace(s), "/min")
	perMin, err := strconv.ParseFloat(n, 64)
	if err != nil || perMin < 0 {
		return gateway.Limit{}, fmt.Errorf("%q: want N/min", s)
	}
	if perMin > 0 && burst < 1 {
		return gateway.Limit{}, fmt.Errorf("burst must be at least 1")
	}
	return gateway.Limit{PerMinute: perMin, Burst: burst}, nil
}

func readTestGrants(path string) ([]gatewayapi.Grant, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var grants []gatewayapi.Grant
	if err := json.Unmarshal(data, &grants); err != nil {
		return nil, fmt.Errorf("%s: want a JSON array of {\"ops\": [...], \"sessions\": [...]}: %w", path, err)
	}
	valid := gateway.ValidGrants(grants)
	if len(valid) != len(grants) {
		log.Printf("WARNING: %s: ignoring %d invalid grant(s)", path, len(grants)-len(valid))
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("%s: no valid grants", path)
	}
	return valid, nil
}

var (
	tailnetV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailnetV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// checkListenAddr refuses anything but a tailnet address, so the gateway can
// never be reachable from the LAN or the internet. Loopback is allowed only
// in test-grants mode, which has no identity check and so must not be
// reachable from anywhere else; in that mode only loopback is allowed.
func checkListenAddr(listen string, testGrants bool) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("-listen %q: want ADDR:PORT: %w", listen, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("-listen %q: host must be an IP address (a tailnet address): %w", listen, err)
	}
	ip = ip.Unmap()
	switch {
	case testGrants:
		if !ip.IsLoopback() {
			return fmt.Errorf("-insecure-test-grants only serves loopback; %s is not", ip)
		}
	case ip.IsLoopback():
		return fmt.Errorf("-listen %s: loopback is only allowed with -insecure-test-grants (there is no tailnet identity on loopback)", ip)
	case !tailnetV4.Contains(ip) && !tailnetV6.Contains(ip):
		return fmt.Errorf("-listen %s is not a tailnet address (100.64.0.0/10 or fd7a:115c:a1e0::/48); the gateway only binds to the tailnet", ip)
	}
	return nil
}

func tailnetIPv4(tailscale string) (string, error) {
	out, err := exec.Command(tailscale, "ip", "-4").Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if _, err := netip.ParseAddr(line); err != nil {
		return "", fmt.Errorf("unexpected `tailscale ip -4` output %q", line)
	}
	return line, nil
}

func runGatewayInstall(args []string) {
	fs := flag.NewFlagSet("gateway install", flag.ExitOnError)
	listen := fs.String("listen", "", "address the service listens on (default: this host's tailnet IPv4, resolved each start)")
	capability := fs.String("capability", "", "app capability name the tailnet policy grants (required)")
	socket := fs.String("socket", "", "Unix socket agentmuxd is listening on (default: the platform's)")
	tailscale := fs.String("tailscale", "", "tailscale CLI binary (default: found on PATH)")
	runUser := fs.String("run-user", "", "Linux: OS user the service runs as, normally the one that owns the sessions (default: auto-detected like doctor)")
	bin := fs.String("bin", defaultGatewayBin, "agentmux binary path the service execs (macOS: ~/.agentmux/bin/agentmux)")
	print := fs.Bool("print", false, "print the unit file / plist without installing it")
	fs.Parse(args)

	if *capability == "" {
		log.Fatal("gateway install: -capability is required")
	}
	if *listen != "" {
		if err := checkListenAddr(*listen, false); err != nil {
			log.Fatalf("gateway install: %v", err)
		}
	}
	identity, err := doctorIdentity(*runUser)
	if err != nil {
		log.Fatalf("gateway install: %v", err)
	}
	binPath := *bin
	if runtime.GOOS == "darwin" && binPath == defaultGatewayBin {
		binPath = filepath.Join(identity.HomeDir, ".agentmux", "bin", "agentmux")
	}
	ts := *tailscale
	if ts == "" {
		// A service starts with a bare PATH, so record where the CLI is now.
		ts = findTailscale()
	}
	runArgs := gatewayRunArgs(*listen, *capability, *socket, ts)
	if err := installGatewayService(identity.Username, identity.HomeDir, binPath, runArgs, *print); err != nil {
		log.Fatalf("gateway install: %v", err)
	}
}

// gatewayRunArgs is the argument list after the binary: gateway run ...
func gatewayRunArgs(listen, capability, socket, tailscale string) []string {
	args := []string{"gateway", "run", "-capability", capability}
	if listen != "" {
		args = append(args, "-listen", listen)
	}
	if socket != "" {
		args = append(args, "-socket", socket)
	}
	if tailscale != "" {
		args = append(args, "-tailscale", tailscale)
	}
	return args
}

func findTailscale() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	const macApp = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
	if _, err := os.Stat(macApp); err == nil {
		return macApp
	}
	return ""
}
