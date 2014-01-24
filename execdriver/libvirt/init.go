// +build linux,dynbinary

package libvirt

import (
	"fmt"
	"github.com/dotcloud/docker/execdriver"
	"github.com/dotcloud/docker/pkg/rpcfd"
	"github.com/dotcloud/docker/utils"
	"log"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"path"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const DriverName = "libvirt"
const SocketPath = "/.dockersocket"
const RpcSocketName = "rpc.sock"

type State int32

const (
	Initial State = iota
	Running
	Exited
	FailedToStart
)

func init() {
	execdriver.RegisterInitFunc(DriverName, sysInit)
}

func rpcSocketPath() string {
	return path.Join(SocketPath, RpcSocketName)
}

type DockerInitState struct {
	State    State
	Error    string
	ExitCode int
}

type DockerInit struct {
	sync.Mutex
	DockerInitState
	resume      chan int
	cancel      chan int // Do we need this?
	stateChange chan int
	process     *os.Process
	processLock chan struct{}

	stdin     *os.File
	stdout    *os.File
	stderr    *os.File
	ptyMaster *os.File
}

func isTty(fd uintptr) bool {
	var termios syscall.Termios
	r1, _, _ := syscall.Syscall(syscall.SYS_IOCTL,
		fd, uintptr(syscall.TCGETS),
		uintptr(unsafe.Pointer(&termios)))
	return r1 == 0
}

// RPC: Get current State
func (init *DockerInit) GetState(_ int, state *DockerInitState) error {
	init.Lock()
	defer init.Unlock()

	*state = init.DockerInitState
	return nil
}

func (init *DockerInit) SetStdin(fd rpcfd.RpcFd, _ *int) error {
	syscall.Dup3(int(fd.Fd), syscall.Stdin, syscall.O_CLOEXEC)
	syscall.Close(int(fd.Fd))
	return nil
}

func (init *DockerInit) SetStdout(fd rpcfd.RpcFd, _ *int) error {
	syscall.Dup3(int(fd.Fd), syscall.Stdout, syscall.O_CLOEXEC)
	syscall.Close(int(fd.Fd))

	return nil
}

func (init *DockerInit) SetStderr(fd rpcfd.RpcFd, _ *int) error {
	syscall.Dup3(int(fd.Fd), syscall.Stderr, syscall.O_CLOEXEC)
	syscall.Close(int(fd.Fd))
	return nil
}

func (init *DockerInit) GetPid(_ int, pid *rpcfd.RpcPid) error {
	init.Lock()
	defer init.Unlock()

	if init.process == nil {
		return fmt.Errorf("Process not yet started")
	}

	pid.Pid = uintptr(syscall.Getpid())
	return nil
}

// RPC: Resume container start or container exit
func (init *DockerInit) Resume(_ int, _ *int) error {
	init.Lock()
	defer init.Unlock()

	init.resume <- 1
	return nil
}

// RPC: Wait for dockerinit state to change
//
// To avoid races, the caller supplies what he thinks is the current
// state and this will return directly if that is not the current state.
//
// For machine containers that have their own init, this function
// doesn't actually return the Exit status (as it can't monitor pid
// 1). Instead the init process (pid 1) will die, which will
// automatically kill all the other container tasks, including the
// non-pid-1 dockerinit.  Docker's RPC call will return an error and
// we assume the container is now dead.
func (init *DockerInit) WaitForStateChange(current State, state *DockerInitState) error {
	init.Lock()
	defer init.Unlock()

	for init.State == current {
		init.Unlock() // Allow calls while waiting
		select {
		case <-init.stateChange:
		case <-init.cancel:
		}
		init.Lock()
	}

	*state = init.DockerInitState
	return nil
}

func (init *DockerInit) setState(state State, err error, exitCode int) {
	init.State = state
	if err != nil {
		init.Error = err.Error()
	} else {
		init.Error = ""
	}
	init.ExitCode = exitCode

	// Non-blocking send
	select {
	case init.stateChange <- 1:
	default:
	}
}

// RPC: Send a signal to the container app
func (init *DockerInit) Signal(signal syscall.Signal, _ *int) error {
	<-init.processLock // Wait until we have a process
	init.Lock()
	defer init.Unlock()
	return init.process.Signal(signal)
}

// Serve RPC commands over a UNIX socket
func rpcServer(init *DockerInit) {

	if err := rpc.Register(init); err != nil {
		log.Fatal(err)
	}

	os.Remove(rpcSocketPath())
	addr := &net.UnixAddr{Net: "unix", Name: rpcSocketPath()}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		log.Fatal(err)
	}

	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			log.Printf("rpc socket accept error: %s", err)
			continue
		}

		rpcfd.ServeConn(conn)

		conn.Close()

		// The RPC connection has closed, which means the docker daemon
		// exited.  Cancel the Wait() call.
		init.cancel <- 1
	}
}

func (init *DockerInit) waitForResume() error {
	select {
	case <-init.resume:
		break
	case <-time.After(1 * time.Second):
		return fmt.Errorf("timeout waiting for docker Resume()")
	}
	return nil
}

func dockerInitNew(args *execdriver.InitArgs) *DockerInit {
	return &DockerInit{
		resume:      make(chan int),
		stateChange: make(chan int, 1),
		cancel:      make(chan int),
		processLock: make(chan struct{}),
	}
}

func start(args *execdriver.InitArgs) (*exec.Cmd, error) {
	// Prepare the cmd based on the given args
	cmdPath, err := exec.LookPath(args.Args[0])
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(cmdPath, args.Args[1:]...)
	cmd.Dir = args.WorkDir

	// App runs in its own session
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if isTty(os.Stdout.Fd()) {
		cmd.SysProcAttr.Setctty = true
	}

	if err := setupHostname(args); err != nil {
		return nil, err
	}

	if err := setupNetworking(args); err != nil {
		return nil, err
	}

	if err := setupCgroups(args); err != nil {
		return nil, err
	}

	if err := setupCapabilities(args); err != nil {
		return nil, err
	}

	// Update uid/gid credentials if needed
	credential, err := getCredential(args)
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr.Credential = credential

	// FIXME: Workaround for libvirt "/.oldroot" directory leak
	// https://bugzilla.redhat.com/show_bug.cgi?id=1026814
	os.Remove("/.oldroot")

	// Start the app
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func monitor(process *os.Process) int {
	// Wait for the app to exit.
	// Also, as pid 1 it's our job to reap all orphaned zombies.
	// We also forward all signals to the app.

	sigchan := make(chan os.Signal, 1)
	utils.CatchAll(sigchan)

	var wstatus syscall.WaitStatus
	var rusage syscall.Rusage

	// Reap any children that may have happened before we set up the signal handlers
	for {
		pid, err := syscall.Wait4(-1, &wstatus, syscall.WNOHANG, &rusage)
		if err == nil && pid == process.Pid {
			return wstatus.ExitStatus()
		}
		if pid <= 0 {
			break
		}
	}

	for sig := range sigchan {
		if sig == syscall.SIGCHLD {
			for {
				pid, err := syscall.Wait4(-1, &wstatus, syscall.WNOHANG, &rusage)
				if err == nil && pid == process.Pid {
					return wstatus.ExitStatus()
				}
				if pid <= 0 {
					break
				}
			}
		} else {
			process.Signal(sig)
		}
	}

	return -1

}

func sysInit(args *execdriver.InitArgs) error {
	init := dockerInitNew(args)

	go rpcServer(init)

	// Initial state, wait for resume
	// This gives docker a chance to get the console FDs before we
	// start so that it won't miss any console output.
	if err := init.waitForResume(); err != nil {
		return err
	}

	// Now that our servers have been started, unmount the socket directory
	// to prevent the container app from trying to impersonate dockerinit.
	syscall.Unmount(SocketPath, syscall.MNT_DETACH)

	init.Lock()
	defer init.Unlock()

	cmd, err := start(args)
	if err != nil {
		init.setState(FailedToStart, err, -1)
	} else {
		init.process = cmd.Process

		// Started the process, now monitor it
		init.setState(Running, nil, -1)

		init.Unlock() // Allow calls while waiting
		exitCode := monitor(init.process)
		init.Lock()

		// Update the exit code for Wait() and detect timeout if Wait() hadn't
		// been called
		init.setState(Exited, nil, exitCode)
	}

	// Wait for docker to call Resume() again.  This gives docker a chance
	// to get the exit code from the RPC socket call interface before we
	// die.
	init.Unlock() // Allow calls while waiting
	if err := init.waitForResume(); err != nil {
		return err
	}
	init.Lock()

	os.Exit(init.ExitCode)

	return nil
}
