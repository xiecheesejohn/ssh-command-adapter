//go:build darwin || linux

// ssh-command-adapter serves SSH over stdin/stdout for use as ProxyCommand.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

type target struct {
	Command []string `json:"command"`
}

type config struct {
	HostKey        string            `json:"host_key"`
	AuthorizedKeys string            `json:"authorized_keys"`
	Targets        map[string]target `json:"targets"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ssh-command-adapter:", err)
		os.Exit(1)
	}
}

func run() error {
	base, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	defaultDir := filepath.Join(base, "ssh-command-adapter")
	if len(os.Args) > 1 && os.Args[1] == "init" {
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		dir := fs.String("dir", defaultDir, "private directory for keys and configuration")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("init takes no positional arguments")
		}
		return initialize(*dir)
	}
	fs := flag.NewFlagSet("ssh-command-adapter", flag.ContinueOnError)
	path := fs.String("config", filepath.Join(defaultDir, "config.json"), "trusted local configuration")
	name := fs.String("target", "", "configured target name (also the SSH username)")
	stdio := fs.Bool("stdio", false, "serve one SSH connection over stdin/stdout; no listening socket")
	check := fs.Bool("check", false, "validate configuration without starting a session")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	cfg, err := readConfig(*path)
	if err != nil {
		return err
	}
	scfg, err := serverConfig(cfg, *name)
	if err != nil {
		return err
	}
	if *check {
		fmt.Fprintln(os.Stderr, "configuration valid")
		return nil
	}
	if !*stdio {
		return errors.New("use --stdio from an SSH ProxyCommand; no network listener is provided")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return serve(ctx, &stdioConn{os.Stdin, os.Stdout}, scfg, cfg.Targets[*name])
}

func readConfig(path string) (*config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cfg config
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return nil, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, errors.New("configuration must contain one JSON object")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	for _, p := range []*string{&cfg.HostKey, &cfg.AuthorizedKeys} {
		if *p == "" {
			return nil, errors.New("host_key and authorized_keys are required")
		}
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	if len(cfg.Targets) == 0 {
		return nil, errors.New("at least one target is required")
	}
	for name, t := range cfg.Targets {
		if !validName(name) {
			return nil, errors.New("target names may contain only letters, digits, dash, underscore and dot")
		}
		if len(t.Command) == 0 || t.Command[0] == "" {
			return nil, fmt.Errorf("target %q has no command", name)
		}
		for _, arg := range t.Command {
			if strings.ContainsRune(arg, 0) {
				return nil, errors.New("NUL in command argument")
			}
		}
		if _, err := exec.LookPath(t.Command[0]); err != nil {
			return nil, fmt.Errorf("target %q executable: %w", name, err)
		}
	}
	return &cfg, nil
}

func validName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func serverConfig(cfg *config, name string) (*ssh.ServerConfig, error) {
	if _, ok := cfg.Targets[name]; !ok {
		return nil, errors.New("--target must name a configured target")
	}
	data, err := os.ReadFile(cfg.HostKey)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse host key: %w", err)
	}
	data, err = os.ReadFile(cfg.AuthorizedKeys)
	if err != nil {
		return nil, err
	}
	var allowed []ssh.PublicKey
	for len(data) > 0 {
		key, _, options, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse authorized keys: %w", err)
		}
		if len(options) > 0 {
			return nil, errors.New("authorized key options are not supported")
		}
		allowed = append(allowed, key)
		data = rest
	}
	if len(allowed) == 0 {
		return nil, errors.New("no authorized keys")
	}
	sc := &ssh.ServerConfig{ServerVersion: "SSH-2.0-ssh-command-adapter", MaxAuthTries: 3}
	sc.PublicKeyCallback = func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if conn.User() == name {
			for _, candidate := range allowed {
				if string(candidate.Marshal()) == string(key.Marshal()) {
					return &ssh.Permissions{}, nil
				}
			}
		}
		return nil, errors.New("key or target rejected")
	}
	sc.AddHostKey(signer)
	return sc, nil
}

type pipeAddr string

func (a pipeAddr) Network() string { return "stdio" }
func (a pipeAddr) String() string  { return string(a) }

type stdioConn struct{ in, out *os.File }

func (c *stdioConn) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.out.Write(p) }
func (c *stdioConn) Close() error                { _ = c.in.Close(); return c.out.Close() }
func (c *stdioConn) LocalAddr() net.Addr         { return pipeAddr("adapter") }
func (c *stdioConn) RemoteAddr() net.Addr        { return pipeAddr("client") }
func (c *stdioConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
func (c *stdioConn) SetReadDeadline(t time.Time) error  { return c.in.SetReadDeadline(t) }
func (c *stdioConn) SetWriteDeadline(t time.Time) error { return c.out.SetWriteDeadline(t) }

func initialize(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	// Never overwrite an existing directory or rotate existing keys implicitly.
	if err := os.Mkdir(abs, 0700); err != nil {
		return fmt.Errorf("create private state directory (must not exist): %w", err)
	}
	host, err := writeKey(filepath.Join(abs, "host_key"))
	if err != nil {
		return err
	}
	client, err := writeKey(filepath.Join(abs, "client_key"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(abs, "authorized_keys"), ssh.MarshalAuthorizedKey(client), 0600); err != nil {
		return err
	}
	// tssh versions that do not honor HostKeyAlias verify HostName instead.
	// This is a private known_hosts file dedicated to this one adapter.
	if err := os.WriteFile(filepath.Join(abs, "known_hosts"), append([]byte("ssh-command-adapter,localhost "), ssh.MarshalAuthorizedKey(host)...), 0600); err != nil {
		return err
	}
	cfg := config{HostKey: "host_key", AuthorizedKeys: "authorized_keys", Targets: map[string]target{"shell": {Command: []string{"/bin/bash", "-l"}}}}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(abs, "config.json"), append(data, '\n'), 0600); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	text := fmt.Sprintf(`Host command-shell
    HostName localhost
    User shell
    ProxyCommand %s --stdio --config %s --target shell
    IdentityFile %s
    IdentitiesOnly yes
    IdentityAgent none
    PreferredAuthentications publickey
    HostKeyAlias ssh-command-adapter
    UserKnownHostsFile %s
    StrictHostKeyChecking yes
    ControlMaster no
    ControlPath none
    RequestTTY force
    RemoteCommand none
    #!! EnableTrzsz no
    #!! EnableZmodem no
    #!! EnableDragFile no
    #!! EnableOSC52 no
    #!! ExpectCount 0
`, shellQuote(exe), shellQuote(filepath.Join(abs, "config.json")), sshQuote(filepath.Join(abs, "client_key")), sshQuote(filepath.Join(abs, "known_hosts")))
	if err := os.WriteFile(filepath.Join(abs, "ssh_config"), []byte(text), 0600); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Created private configuration:", abs)
	fmt.Fprintln(os.Stderr, "Try: tssh -F", shellQuote(filepath.Join(abs, "ssh_config")), "command-shell")
	return nil
}

func writeKey(path string) (ssh.PublicKey, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(private, "ssh-command-adapter")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		return nil, err
	}
	pub, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		return nil, err
	}
	return pub, os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(pub), 0600)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func sshQuote(s string) string {
	return "\"" + strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "\"", "\\\"") + "\""
}
