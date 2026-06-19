package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type LaunchConfig struct {
	ChromePath       string
	UserDataDir      string
	ProfileDirectory string
	Port             int
	Extensions       []string
	Args             []string
}

type Launcher struct {
	cmd      *exec.Cmd
	endpoint string
	port     int
}

func Launch(ctx context.Context, cfg LaunchConfig) (*Launcher, error) {
	chromePath, err := FindChrome(cfg.ChromePath)
	if err != nil {
		return nil, err
	}
	if cfg.UserDataDir == "" {
		cfg.UserDataDir = DefaultProfileDir("")
	}
	if err := os.MkdirAll(cfg.UserDataDir, 0o700); err != nil {
		return nil, err
	}
	port := cfg.Port
	if port == 0 {
		port, err = freePort()
		if err != nil {
			return nil, err
		}
	}

	args := []string{
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=" + strconv.Itoa(port),
		"--user-data-dir=" + cfg.UserDataDir,
		"--no-first-run",
		"--no-default-browser-check",
		// Keep in-page timers (setTimeout/setInterval) firing at their requested
		// rate. Chrome throttles timers to ~1Hz on hidden/occluded/headless
		// pages, which silently turned the 100ms actionability poll into a
		// ~700-900ms stall per click. These flags are standard for an automation
		// browser and only affect background-throttling, never foreground tabs.
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
	}
	if cfg.ProfileDirectory != "" {
		args = append(args, "--profile-directory="+cfg.ProfileDirectory)
	}
	if len(cfg.Extensions) > 0 {
		args = append(args, "--load-extension="+strings.Join(cfg.Extensions, ","))
	}
	args = append(args, cfg.Args...)
	args = append(args, "about:blank")

	cmd := exec.CommandContext(ctx, chromePath, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	launcher := &Launcher{cmd: cmd, endpoint: fmt.Sprintf("http://127.0.0.1:%d", port), port: port}
	if err := launcher.waitReady(ctx, 15*time.Second); err != nil {
		_ = launcher.Close()
		return nil, err
	}
	return launcher, nil
}

func (l *Launcher) Endpoint() string {
	return l.endpoint
}

func (l *Launcher) Port() int {
	return l.port
}

func (l *Launcher) Close() error {
	if l == nil || l.cmd == nil || l.cmd.Process == nil {
		return nil
	}
	if err := l.cmd.Process.Signal(os.Interrupt); err != nil {
		return l.cmd.Process.Kill()
	}
	done := make(chan error, 1)
	go func() { done <- l.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		return l.cmd.Process.Kill()
	}
}

func (l *Launcher) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := probe(deadline, l.endpoint); err == nil {
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("Chrome CDP endpoint did not become ready at %s: %w", l.endpoint, deadline.Err())
		case <-ticker.C:
		}
	}
}

func probe(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/version", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	var payload struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	if payload.WebSocketDebuggerURL == "" {
		return fmt.Errorf("missing webSocketDebuggerUrl")
	}
	return nil
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
