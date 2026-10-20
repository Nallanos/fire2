package shim

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
)

// killGrace is how long the shim waits for Firecracker to exit after
// forwarding SIGTERM, before killing it.
const killGrace = 3 * time.Second

// Main runs a shim with command-line args (without argv[0]) and returns its
// exit code. cmd/fire2-shim is just os.Exit(shim.Main(os.Args[1:])).
func Main(args []string) int {
	fs := flag.NewFlagSet(ProcessName, flag.ContinueOnError)
	dir := fs.String("dir", "", "sandbox directory")
	firecracker := fs.String("firecracker", "", "firecracker binary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || *firecracker == "" {
		log.Printf("%s: -dir and -firecracker are required", ProcessName)
		return 2
	}
	if err := serve(*dir, *firecracker); err != nil {
		log.Printf("%s: %v", ProcessName, err)
		return 1
	}
	return 0
}

// serve starts Firecracker and reports its exit to every client, then
// returns.
func serve(dir, firecracker string) error {
	sockPath := Path(dir, SocketFile)
	// Listen before starting the VM, so the worker can always connect.
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer os.Remove(sockPath)
	if err := writePidFile(dir); err != nil {
		ln.Close()
		return err
	}

	s := &server{}
	go s.accept(ln)

	// SIGTERM (from the worker) stops the VM gracefully, SIGKILL after a
	// grace period. Registered before Firecracker starts so none is lost.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)

	procCh := make(chan *os.Process, 1)
	exitCh := make(chan int, 1)
	startErr := make(chan error, 1)
	go runFirecracker(dir, firecracker, procCh, exitCh, startErr)
	if err := <-startErr; err != nil {
		s.finish(exitedLine(-1))
		ln.Close()
		return err
	}
	proc := <-procCh

	var code int
	var killTimer <-chan time.Time
wait:
	for {
		select {
		case code = <-exitCh:
			break wait
		case sig := <-signals:
			log.Printf("%s: %s, stopping firecracker", ProcessName, sig)
			_ = proc.Signal(syscall.SIGTERM)
			if killTimer == nil {
				killTimer = time.After(killGrace)
			}
		case <-killTimer:
			log.Printf("%s: firecracker still running after %s, killing it", ProcessName, killGrace)
			_ = proc.Kill()
		}
	}

	s.finish(exitedLine(code))
	ln.Close()
	return nil
}

// runFirecracker starts Firecracker and waits for it on a locked OS thread.
// Pdeathsig fires when the *thread* that forked the child exits, not the
// process: the goroutine keeps its thread for Firecracker's whole life, and
// the kernel then kills the VM only when the shim itself dies.
func runFirecracker(dir, firecracker string, procCh chan<- *os.Process, exitCh chan<- int, startErr chan<- error) {
	runtime.LockOSThread() // never unlocked: the thread dies with this goroutine

	apiSock := Path(dir, APISocket)
	_ = os.Remove(apiSock)
	logFile, err := os.OpenFile(Path(dir, VMLogFile), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		startErr <- fmt.Errorf("open vm log: %w", err)
		return
	}
	defer logFile.Close()

	cmd := exec.Command(firecracker, "--api-sock", apiSock, "--config-file", Path(dir, ConfigFile))
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		startErr <- fmt.Errorf("start firecracker: %w", err)
		return
	}
	startErr <- nil
	procCh <- cmd.Process

	err = cmd.Wait()
	exitCh <- exitCode(err)
}

// exitCode follows the shell convention: 128+signal for a killed process.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exitErr.ExitCode()
	}
	return -1
}

func writePidFile(dir string) error {
	tmp := Path(dir, PidFile+".tmp")
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	if err := os.Rename(tmp, Path(dir, PidFile)); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	return nil
}

// server tracks connected clients until Firecracker exits.
type server struct {
	mu      sync.Mutex
	clients []net.Conn
	final   string // set once Firecracker exited
}

func (s *server) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		s.mu.Lock()
		if s.final != "" {
			// Connected between the exit and the listener closing.
			s.mu.Unlock()
			_, _ = conn.Write([]byte(s.final))
			conn.Close()
			continue
		}
		s.clients = append(s.clients, conn)
		s.mu.Unlock()
	}
}

// finish sends line to every client and disconnects them.
func (s *server) finish(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.final = line
	for _, c := range s.clients {
		_ = c.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = c.Write([]byte(line))
		c.Close()
	}
	s.clients = nil
}
