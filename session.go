//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func serve(ctx context.Context, raw net.Conn, cfg *ssh.ServerConfig, target target) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer raw.Close()
	// Cancellation also interrupts a stalled handshake.
	go func() { <-ctx.Done(); _ = raw.Close() }()
	conn, channels, requests, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return fmt.Errorf("SSH handshake: %w", err)
	}
	defer conn.Close()
	go func() {
		for req := range requests {
			_ = req.Reply(req.Type == "keepalive@openssh.com" || req.Type == "no-more-sessions@openssh.com", nil)
		}
	}()
	var wg sync.WaitGroup
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			_ = incoming.Reject(ssh.UnknownChannelType, "only interactive session channels are supported")
			continue
		}
		ch, reqs, err := incoming.Accept()
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); handleSession(ctx, ch, reqs, target) }()
	}
	cancel()
	wg.Wait()
	return nil
}

type ptyRequest struct {
	Term                                     string
	Columns, Rows, WidthPixels, HeightPixels uint32
	Modes                                    string
}
type windowRequest struct{ Columns, Rows, WidthPixels, HeightPixels uint32 }

func dimensions(w windowRequest) (*pty.Winsize, error) {
	if w.Rows == 0 || w.Columns == 0 || w.Rows > 65535 || w.Columns > 65535 || w.WidthPixels > 65535 || w.HeightPixels > 65535 {
		return nil, errors.New("invalid PTY dimensions")
	}
	return &pty.Winsize{Rows: uint16(w.Rows), Cols: uint16(w.Columns), X: uint16(w.WidthPixels), Y: uint16(w.HeightPixels)}, nil
}

func handleSession(parent context.Context, ch ssh.Channel, requests <-chan *ssh.Request, target target) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer ch.Close()
	var master, slave *os.File
	var cmd *exec.Cmd
	var terminal string
	var done <-chan struct{}
	env := []string{}
	defer func() {
		cancel()
		if master != nil {
			_ = master.Close()
		}
		if slave != nil {
			_ = slave.Close()
		}
		if done != nil {
			<-done
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case req, ok := <-requests:
			if !ok {
				return
			}
			accepted := false
			switch req.Type {
			case "pty-req":
				var p ptyRequest
				if master != nil || cmd != nil || ssh.Unmarshal(req.Payload, &p) != nil || len(p.Term) > 256 || strings.ContainsRune(p.Term, 0) {
					break
				}
				size, err := dimensions(windowRequest{p.Columns, p.Rows, p.WidthPixels, p.HeightPixels})
				if err != nil {
					break
				}
				m, s, err := pty.Open()
				if err != nil {
					break
				}
				// The child is itself an interactive terminal client. Raw transport
				// avoids a second echo/CRLF layer and preserves binary protocol bytes.
				if _, err = term.MakeRaw(int(s.Fd())); err == nil {
					err = pty.Setsize(m, size)
				}
				if err == nil {
					m, err = pollablePTY(m)
				}
				if err != nil {
					_ = m.Close()
					_ = s.Close()
					break
				}
				master, slave, terminal, accepted = m, s, p.Term, true
			case "window-change":
				var w windowRequest
				if master == nil || ssh.Unmarshal(req.Payload, &w) != nil {
					break
				}
				size, err := dimensions(w)
				accepted = err == nil && pty.Setsize(master, size) == nil
			case "env":
				var e struct{ Name, Value string }
				if cmd != nil || ssh.Unmarshal(req.Payload, &e) != nil || len(e.Value) > 1024 || strings.ContainsRune(e.Value, 0) {
					break
				}
				if e.Name == "LANG" || e.Name == "LC_ALL" || e.Name == "LC_CTYPE" {
					env = append(env, e.Name+"="+e.Value)
					accepted = true
				}
			case "shell":
				if master == nil || cmd != nil || len(req.Payload) != 0 {
					break
				}
				cmd = exec.Command(target.Command[0], target.Command[1:]...)
				cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
				cmd.Env = childEnvironment(terminal, env)
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
				if err := cmd.Start(); err != nil {
					_, _ = io.WriteString(ch.Stderr(), "adapter: failed to start configured command\r\n")
					cmd = nil
					break
				}
				_ = slave.Close()
				slave = nil
				finished := make(chan struct{})
				done = finished
				go runCommand(ctx, ch, master, cmd, finished)
				accepted = true
			case "signal":
				var p struct{ Signal string }
				if cmd != nil && ssh.Unmarshal(req.Payload, &p) == nil {
					if sig, ok := commandSignals[p.Signal]; ok {
						accepted = syscall.Kill(-cmd.Process.Pid, sig) == nil
					}
				}
			case "exec", "subsystem":
				_, _ = io.WriteString(ch.Stderr(), "adapter: interactive PTY shell only; exec, SFTP and SCP are unsupported\r\n")
			}
			_ = req.Reply(accepted, nil)
		}
	}
}

var commandSignals = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT,
	"TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2,
}

// pty.Open returns an os.NewFile opened in blocking mode on Darwin. Re-wrap
// a duplicate that is already nonblocking so Go registers it with the poller.
// Its Fd method then also preserves nonblocking mode during resize ioctls.
func pollablePTY(original *os.File) (*os.File, error) {
	fd, err := syscall.Dup(int(original.Fd()))
	if err != nil {
		return original, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return original, err
	}
	replacement := os.NewFile(uintptr(fd), "adapter-pty")
	_ = original.Close()
	return replacement, nil
}

func childEnvironment(terminal string, extra []string) []string {
	env := []string{}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if name == "TERM" || name == "TMUX" || name == "TMUX_PANE" || name == "STY" || strings.HasPrefix(name, "SSH_") {
			continue
		}
		env = append(env, value)
	}
	return append(append(env, "TERM="+terminal), extra...)
}

func runCommand(ctx context.Context, ch ssh.Channel, master *os.File, cmd *exec.Cmd, done chan<- struct{}) {
	defer close(done)
	exited := make(chan struct{})
	inputDone := make(chan struct{})
	outputDone := make(chan struct{})
	go func() { _, _ = io.Copy(master, ch); close(inputDone) }()
	go func() { _, _ = io.Copy(ch, master); close(outputDone) }()
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		select {
		case <-ctx.Done():
		case <-inputDone:
		case <-exited:
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-exited:
		case <-timer.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	err := cmd.Wait()
	close(exited)
	<-cleanupDone // No process signals may be sent after Wait completes.
	// Drain normal output before reporting exit, with a bound for descendants
	// that keep the slave PTY open after the configured command exits.
	timer := time.NewTimer(time.Second)
	select {
	case <-outputDone:
	case <-ctx.Done():
	case <-timer.C:
	}
	timer.Stop()
	_ = master.Close()
	code := uint32(0)
	if err != nil {
		code = 1
		if e, ok := err.(*exec.ExitError); ok {
			if s, ok := e.Sys().(syscall.WaitStatus); ok && s.Signaled() {
				code = uint32(128 + s.Signal())
			} else if e.ExitCode() >= 0 {
				code = uint32(e.ExitCode())
			}
		}
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
	_ = ch.Close()
	<-inputDone
}
