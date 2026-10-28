//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

// The child is a deterministic terminal client, never a real external host.
func TestPTYHelper(t *testing.T) {
	if os.Getenv("ADAPTER_TEST_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "size":
		changes := make(chan os.Signal, 4)
		signal.Notify(changes, syscall.SIGWINCH)
		printSize := func() {
			s, err := pty.GetsizeFull(os.Stdin)
			if err != nil {
				os.Exit(90)
			}
			fmt.Printf("SIZE=%dx%d\n", s.Rows, s.Cols)
		}
		printSize()
		for range changes {
			printSize()
		}
	case "binary":
		payload := make([]byte, 256)
		if _, err := io.ReadFull(os.Stdin, payload); err != nil {
			os.Exit(91)
		}
		_, _ = os.Stdout.Write(payload)
	case "wait":
		fmt.Printf("PID=%d\n", os.Getpid())
		for {
			time.Sleep(time.Hour)
		}
	case "exit":
		fmt.Print("final-output\n")
		os.Exit(37)
	}
	os.Exit(0)
}

func localPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	var conns [2]net.Conn
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "test-connection")
		conns[i], err = net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return conns[0], conns[1]
}

func fixture(t *testing.T, mode string) (*ssh.Client, <-chan error) {
	return fixtureAuth(t, mode, false)
}

func fixtureAuth(t *testing.T, mode string, noClientAuth bool) (*ssh.Client, <-chan error) {
	t.Helper()
	t.Setenv("ADAPTER_TEST_HELPER", "1")
	dir := filepath.Join(t.TempDir(), "private")
	if err := initialize(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Targets["shell"] = target{Command: []string{os.Args[0], "-test.run=^TestPTYHelper$", "--", mode}}
	if noClientAuth {
		cfg.AuthorizedKeys = ""
	}
	scfg, err := serverConfig(cfg, "shell", noClientAuth)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(filepath.Join(dir, "client_key"))
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	server, client := localPair(t)
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	go func() { result <- serve(ctx, server, scfg, cfg.Targets["shell"]) }()
	var auth []ssh.AuthMethod
	if !noClientAuth {
		auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	}
	conn, channels, requests, err := ssh.NewClientConn(client, "test", &ssh.ClientConfig{
		User: "shell", Auth: auth,
		HostKeyCallback: ssh.FixedHostKey(scfgHostKey(t, cfg.HostKey)),
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := ssh.NewClient(conn, channels, requests)
	t.Cleanup(func() { _ = c.Close(); cancel() })
	return c, result
}

func scfgHostKey(t *testing.T, path string) ssh.PublicKey {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

func shellSession(t *testing.T, c *ssh.Client) (*ssh.Session, io.WriteCloser, io.Reader) {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	in, err := s.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	return s, in, out
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() { text, err := r.ReadString('\n'); ch <- result{text, err} }()
	select {
	case v := <-ch:
		if v.err != nil {
			t.Fatal(v.err)
		}
		return strings.TrimSpace(v.text)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for terminal output")
	}
	return ""
}

func TestInitialSizeAndRepeatedResize(t *testing.T) {
	c, _ := fixture(t, "size")
	s, _, out := shellSession(t, c)
	r := bufio.NewReader(out)
	if got := readLine(t, r); got != "SIZE=24x80" {
		t.Fatal(got)
	}
	for _, size := range [][2]int{{40, 120}, {17, 63}, {55, 200}} {
		if err := s.WindowChange(size[0], size[1]); err != nil {
			t.Fatal(err)
		}
		if got := readLine(t, r); got != fmt.Sprintf("SIZE=%dx%d", size[0], size[1]) {
			t.Fatal(got)
		}
	}
}

func TestBinaryTransport(t *testing.T) {
	c, _ := fixture(t, "binary")
	s, in, out := shellSession(t, c)
	want := make([]byte, 256)
	for i := range want {
		want[i] = byte(i)
	}
	if _, err := in.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 256)
	if _, err := io.ReadFull(out, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("PTY changed binary bytes")
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestExitStatusAndOutputDrain(t *testing.T) {
	c, _ := fixture(t, "exit")
	s, _, out := shellSession(t, c)
	data, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "final-output\n" {
		t.Fatalf("output: %q", data)
	}
	var exit *ssh.ExitError
	if err := s.Wait(); !errors.As(err, &exit) || exit.ExitStatus() != 37 {
		t.Fatalf("exit: %v", err)
	}
}

func TestDisconnectReapsChild(t *testing.T) {
	c, serverDone := fixture(t, "wait")
	_, _, out := shellSession(t, c)
	line := readLine(t, bufio.NewReader(out))
	pid, err := strconv.Atoi(strings.TrimPrefix(line, "PID="))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not clean up after disconnect")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child still exists: %v", err)
	}
}

func TestRejectExecAndForwarding(t *testing.T) {
	c, _ := fixture(t, "wait")
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Start("arbitrary command"); err == nil {
		t.Fatal("exec was accepted")
	}
	if _, _, err := c.OpenChannel("direct-tcpip", nil); err == nil {
		t.Fatal("forwarding was accepted")
	}
}

func TestKeyAndTargetAuthentication(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := initialize(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	scfg, err := serverConfig(cfg, "shell", false)
	if err != nil {
		t.Fatal(err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	wrongKey, _ := ssh.NewSignerFromKey(other)
	data, _ := os.ReadFile(filepath.Join(dir, "client_key"))
	rightKey, _ := ssh.ParsePrivateKey(data)
	for _, test := range []struct {
		name string
		key  ssh.Signer
	}{{"shell", wrongKey}, {"another-target", rightKey}} {
		server, client := localPair(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		go func() { _ = serve(ctx, server, scfg, cfg.Targets["shell"]) }()
		conn, _, _, err := ssh.NewClientConn(client, "test", &ssh.ClientConfig{User: test.name, Auth: []ssh.AuthMethod{ssh.PublicKeys(test.key)}, HostKeyCallback: ssh.FixedHostKey(scfgHostKey(t, cfg.HostKey))})
		cancel()
		client.Close()
		if err == nil {
			conn.Close()
			t.Fatal("unexpected authentication success")
		}
	}
}

func TestInitializationDoesNotOverwriteKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := initialize(dir); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "host_key"))
	if err := initialize(dir); err == nil {
		t.Fatal("existing state overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "host_key"))
	if !bytes.Equal(before, after) {
		t.Fatal("host key changed")
	}
}

func TestNoClientAuthPreservesTerminalSession(t *testing.T) {
	c, _ := fixtureAuth(t, "exit", true)
	s, _, out := shellSession(t, c)
	data, err := io.ReadAll(out)
	if err != nil || string(data) != "final-output\n" {
		t.Fatalf("output=%q err=%v", data, err)
	}
	var exit *ssh.ExitError
	if err := s.Wait(); !errors.As(err, &exit) || exit.ExitStatus() != 37 {
		t.Fatalf("exit: %v", err)
	}
}

func TestNoClientAuthSeparatesProfileFromDestinationUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := initialize(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthorizedKeys = ""
	scfg, err := serverConfig(cfg, "shell", true)
	if err != nil {
		t.Fatal(err)
	}
	server, client := localPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = serve(ctx, server, scfg, cfg.Targets["shell"]) }()
	defer client.Close()
	conn, _, _, err := ssh.NewClientConn(client, "test", &ssh.ClientConfig{User: "root", HostKeyCallback: ssh.FixedHostKey(scfgHostKey(t, cfg.HostKey))})
	if err != nil {
		t.Fatalf("destination username must not select the local profile: %v", err)
	}
	conn.Close()
	if _, err := serverConfig(cfg, "root", true); err == nil {
		t.Fatal("unknown local command profile was accepted")
	}
}

func TestInitializeWithoutClientKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := initializeMode(dir, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"client_key", "client_key.pub", "authorized_keys"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected %s: %v", name, err)
		}
	}
	cfg, err := readConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serverConfig(cfg, "shell", true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "ssh_config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "IdentityFile") || !strings.Contains(string(data), "--no-client-auth") || !strings.Contains(string(data), "--target shell") {
		t.Fatalf("unexpected generated config: %s", data)
	}
}
