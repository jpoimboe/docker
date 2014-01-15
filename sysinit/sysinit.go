package sysinit

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/dotcloud/docker/pkg/netlink"
	"github.com/dotcloud/docker/rpcfd"
	"github.com/dotcloud/docker/utils"
	"github.com/kr/pty"
	"github.com/syndtr/gocapability/capability"
	"io/ioutil"
	"log"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type DockerInitArgs struct {
	user       string
	gateway    string
	ip         string
	workDir    string
	privileged bool
	tty        bool
	openStdin  bool
	env        []string
	args       []string
	mtu        int
}

const SocketPath = "/.dockersocket"
const RpcSocketName = "rpc.sock"

func rpcSocketPath() string {
	return path.Join(SocketPath, RpcSocketName)
}

type DockerInitRpc struct {
	resume      chan int
	cancel      chan int
	exitCode    chan int
	process     *os.Process
	processLock chan struct{}

	openStdin bool
	stdin     *os.File
	stdout    *os.File
	stderr    *os.File
	ptyMaster *os.File
}

// RPC: Pass pty master FD
func (dockerInitRpc *DockerInitRpc) PtyMaster(_ int, rpcFd *rpcfd.RpcFd) error {
	if dockerInitRpc.ptyMaster == nil {
		return fmt.Errorf("ptyMaster is nil")
	}
	rpcFd.Fd = dockerInitRpc.ptyMaster.Fd()
	return nil
}

// RPC: Pass stdout FD
func (dockerInitRpc *DockerInitRpc) Stdout(_ int, rpcFd *rpcfd.RpcFd) error {
	if dockerInitRpc.stdout == nil {
		return fmt.Errorf("stdout is nil")
	}
	rpcFd.Fd = dockerInitRpc.stdout.Fd()
	return nil
}

// RPC: Pass stderr FD
func (dockerInitRpc *DockerInitRpc) Stderr(_ int, rpcFd *rpcfd.RpcFd) error {
	if dockerInitRpc.stderr == nil {
		return fmt.Errorf("stderr is nil")
	}
	rpcFd.Fd = dockerInitRpc.stderr.Fd()
	return nil
}

// RPC: Pass stdin FD
func (dockerInitRpc *DockerInitRpc) Stdin(_ int, rpcFd *rpcfd.RpcFd) error {
	if dockerInitRpc.stdin == nil {
		return fmt.Errorf("stdin is nil")
	}
	rpcFd.Fd = dockerInitRpc.stdin.Fd()
	return nil
}

// RPC: For StdinOnce mode, allow docker to close dockerinit's reference to
// stdin so that docker can close it later
//
// FIXME: is StdinOnce mode obsolete now that dockerinit can keep stdin open?
func (dockerInitRpc *DockerInitRpc) StdinClose(_ int, _ *int) error {
	if dockerInitRpc.stdin == nil {
		return fmt.Errorf("stdin is nil")
	}
	dockerInitRpc.stdin.Close()
	dockerInitRpc.stdin = nil
	return nil
}

// RPC: Resume container start or container exit
func (dockerInitRpc *DockerInitRpc) Resume(_ int, _ *int) error {
	dockerInitRpc.resume <- 1
	return nil
}

// RPC: Wait for container app exit and return the exit code.
//
// For machine containers that have their own init, this function doesn't
// actually return, but that's ok.  The init process (pid 1) will die, which
// will automatically kill all the other container tasks, including the
// non-pid-1 dockerinit.  Docker's RPC Wait() call will detect that the socket
// closed and return an error.
func (dockerInitRpc *DockerInitRpc) Wait(_ int, exitCode *int) error {
	select {
	case *exitCode = <-dockerInitRpc.exitCode:
	case <-dockerInitRpc.cancel:
		*exitCode = -1
	}
	return nil
}

// RPC: Send a signal to the container app
func (dockerInitRpc *DockerInitRpc) Signal(signal syscall.Signal, _ *int) error {
	<-dockerInitRpc.processLock
	return dockerInitRpc.process.Signal(signal)
}

// Serve RPC commands over a UNIX socket
func rpcServer(dockerInitRpc *DockerInitRpc) {

	if err := rpc.Register(dockerInitRpc); err != nil {
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
		dockerInitRpc.cancel <- 1
	}
}

func setupHostname(args *DockerInitArgs) error {
	hostname := getEnv(args, "HOSTNAME")
	if hostname == "" {
		return nil
	}
	return setHostname(hostname)
}

func setupNetworking(args *DockerInitArgs) error {
	if args.ip != "" {
		// eth0
		iface, err := net.InterfaceByName("eth0")
		if err != nil {
			return fmt.Errorf("Unable to set up networking: %v", err)
		}
		ip, ipNet, err := net.ParseCIDR(args.ip)
		if err != nil {
			return fmt.Errorf("Unable to set up networking: %v", err)
		}
		if err := netlink.NetworkLinkAddIp(iface, ip, ipNet); err != nil {
			return fmt.Errorf("Unable to set up networking: %v", err)
		}
		if err := netlink.NetworkSetMTU(iface, args.mtu); err != nil {
			return fmt.Errorf("Unable to set MTU: %v", err)
		}
		if err := netlink.NetworkLinkUp(iface); err != nil {
			return fmt.Errorf("Unable to set up networking: %v", err)
		}

		// loopback
		if iface, err = net.InterfaceByName("lo"); err != nil {
			return fmt.Errorf("Unable to set up networking: %v", err)
		}
		if err := netlink.NetworkLinkUp(iface); err != nil {
			return fmt.Errorf("Unable to set up networking: %v", err)
		}
	}
	if args.gateway != "" {
		gw := net.ParseIP(args.gateway)
		if gw == nil {
			return fmt.Errorf("Unable to set up networking, %s is not a valid gateway IP", args.gateway)
		}

		if err := netlink.AddDefaultGw(gw); err != nil {
			return fmt.Errorf("Unable to set up networking: %v", err)
		}
	}

	return nil
}

func getCredential(args *DockerInitArgs) (*syscall.Credential, error) {
	if args.user == "" {
		return nil, nil
	}
	userent, err := utils.UserLookup(args.user)
	if err != nil {
		return nil, fmt.Errorf("Unable to find user %v: %v", args.user, err)
	}

	uid, err := strconv.Atoi(userent.Uid)
	if err != nil {
		return nil, fmt.Errorf("Invalid uid: %v", userent.Uid)
	}
	gid, err := strconv.Atoi(userent.Gid)
	if err != nil {
		return nil, fmt.Errorf("Invalid gid: %v", userent.Gid)
	}

	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

func setupCapabilities(args *DockerInitArgs) error {

	if args.privileged {
		return nil
	}

	drop := []capability.Cap{
		capability.CAP_SETPCAP,
		capability.CAP_SYS_MODULE,
		capability.CAP_SYS_RAWIO,
		capability.CAP_SYS_PACCT,
		capability.CAP_SYS_ADMIN,
		capability.CAP_SYS_NICE,
		capability.CAP_SYS_RESOURCE,
		capability.CAP_SYS_TIME,
		capability.CAP_SYS_TTY_CONFIG,
		capability.CAP_MKNOD,
		capability.CAP_AUDIT_WRITE,
		capability.CAP_AUDIT_CONTROL,
		capability.CAP_MAC_OVERRIDE,
		capability.CAP_MAC_ADMIN,
	}

	c, err := capability.NewPid(os.Getpid())
	if err != nil {
		return err
	}

	c.Unset(capability.CAPS|capability.BOUNDS, drop...)

	if err := c.Apply(capability.CAPS | capability.BOUNDS); err != nil {
		return err
	}
	return nil
}

func getEnv(args *DockerInitArgs, key string) string {
	for _, kv := range args.env {
		parts := strings.SplitN(kv, "=", 2)
		if parts[0] == key && len(parts) == 2 {
			return parts[1]
		}
	}
	return ""
}

func getCmdPath(args *DockerInitArgs) (string, error) {

	// Set PATH in dockerinit so we can find the cmd
	if envPath := getEnv(args, "PATH"); envPath != "" {
		os.Setenv("PATH", envPath)
	}

	// Find the cmd
	cmdPath, err := exec.LookPath(args.args[0])
	if err != nil {
		if args.workDir == "" {
			return "", err
		}
		if cmdPath, err = exec.LookPath(path.Join(args.workDir, args.args[0])); err != nil {
			return "", err
		}
	}

	return cmdPath, nil
}

// Start the RPC server and wait for docker to tell us to resume starting the
// container.  This gives docker a chance to get the console FDs before we
// start so that it won't miss any console output.
func startServerAndWait(dockerInitRpc *DockerInitRpc) error {

	go rpcServer(dockerInitRpc)

	select {
	case <-dockerInitRpc.resume:
		break
	case <-time.After(time.Second):
		return fmt.Errorf("timeout waiting for docker Resume()")
	}

	// Now that our servers have been started, unmount the socket directory
	// to prevent the container app from trying to impersonate dockerinit.
	syscall.Unmount(SocketPath, syscall.MNT_DETACH)

	return nil
}

func dockerInitRpcNew(args *DockerInitArgs) *DockerInitRpc {
	return &DockerInitRpc{
		resume:      make(chan int),
		exitCode:    make(chan int),
		cancel:      make(chan int),
		processLock: make(chan struct{}),
		openStdin:   args.openStdin,
	}
}

// Run as pid 1 in the typical Docker usage: an app container that doesn't
// need its own init process.  Running as pid 1 allows us to monitor the
// container app and return its exit code.
func dockerInitApp(args *DockerInitArgs) error {

	// Prepare the cmd based on the given args
	cmdPath, err := getCmdPath(args)
	if err != nil {
		return err
	}
	cmd := exec.Command(cmdPath, args.args[1:]...)
	cmd.Dir = args.workDir
	cmd.Env = args.env

	// App runs in its own session
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Console setup.  Hook up the container app's stdin/stdout/stderr to
	// either a pty or pipes.  The FDs for the controlling side of the
	// pty/pipes will be passed to docker later via rpc.
	dockerInitRpc := dockerInitRpcNew(args)
	if args.tty {
		ptyMaster, ptySlave, err := pty.Open()
		if err != nil {
			return err
		}
		dockerInitRpc.ptyMaster = ptyMaster
		cmd.Stdout = ptySlave
		cmd.Stderr = ptySlave
		if args.openStdin {
			cmd.Stdin = ptySlave
			cmd.SysProcAttr.Setctty = true
		}
	} else {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		dockerInitRpc.stdout = stdout.(*os.File)

		stderr, err := cmd.StderrPipe()
		if err != nil {
			return err
		}
		dockerInitRpc.stderr = stderr.(*os.File)
		if args.openStdin {
			// Can't use cmd.StdinPipe() here, since in Go 1.2 it
			// returns an io.WriteCloser with the underlying object
			// being an *exec.closeOnce, neither of which provides
			// a way to convert to an FD.
			pipeRead, pipeWrite, err := os.Pipe()
			if err != nil {
				return err
			}
			cmd.Stdin = pipeRead
			dockerInitRpc.stdin = pipeWrite
		}
	}

	// Start the RPC server and wait for the resume call from docker
	if err := startServerAndWait(dockerInitRpc); err != nil {
		return err
	}

	if err := setupHostname(args); err != nil {
		return err
	}

	if err := setupNetworking(args); err != nil {
		return err
	}

	if err := setupCapabilities(args); err != nil {
		return err
	}

	// Update uid/gid credentials if needed
	credential, err := getCredential(args)
	if err != nil {
		return err
	}
	cmd.SysProcAttr.Credential = credential

	// Start the app
	if err := cmd.Start(); err != nil {
		return err
	}

	dockerInitRpc.process = cmd.Process
	close(dockerInitRpc.processLock)

	// Forward all signals to the app
	sigchan := make(chan os.Signal, 1)
	utils.CatchAll(sigchan)
	go func() {
		for sig := range sigchan {
			if sig == syscall.SIGCHLD {
				continue
			}
			cmd.Process.Signal(sig)
		}
	}()

	// Wait for the app to exit.  Also, as pid 1 it's our job to reap all
	// orphaned zombies.
	var wstatus syscall.WaitStatus
	for {
		var rusage syscall.Rusage
		pid, err := syscall.Wait4(-1, &wstatus, 0, &rusage)
		if err == nil && pid == cmd.Process.Pid {
			break
		}
	}

	// Update the exit code for Wait() and detect timeout if Wait() hadn't
	// been called
	exitCode := wstatus.ExitStatus()
	select {
	case dockerInitRpc.exitCode <- exitCode:
	case <-time.After(time.Second):
		return fmt.Errorf("timeout waiting for docker Wait()")
	}

	// Wait for docker to call Resume() again.  This gives docker a chance
	// to get the exit code from the RPC socket call interface before we
	// die.
	select {
	case <-dockerInitRpc.resume:
	case <-time.After(time.Second):
		return fmt.Errorf("timeout waiting for docker Resume()")
	}

	os.Exit(exitCode)
	return nil
}

// Sys Init code
// This code is run INSIDE the container and is responsible for setting
// up the environment before running the actual process
func SysInit() {
	if len(os.Args) <= 1 {
		fmt.Println("You should not invoke dockerinit manually")
		os.Exit(1)
	}

	// Get cmdline arguments
	user := flag.String("u", "", "username or uid")
	gateway := flag.String("g", "", "gateway address")
	ip := flag.String("i", "", "ip address")
	workDir := flag.String("w", "", "workdir")
	privileged := flag.Bool("privileged", false, "privileged mode")
	tty := flag.Bool("tty", false, "use pseudo-tty")
	openStdin := flag.Bool("stdin", false, "open stdin")
	mtu := flag.Int("mtu", 1500, "interface mtu")
	flag.Parse()

	// Get env
	var env []string
	content, err := ioutil.ReadFile("/.dockerenv")
	if err != nil {
		log.Fatalf("Unable to load environment variables: %v", err)
	}
	if err := json.Unmarshal(content, &env); err != nil {
		log.Fatalf("Unable to unmarshal environment variables: %v", err)
	}

	// Propagate the plugin-specific container env variable
	env = append(env, "container="+os.Getenv("container"))

	args := &DockerInitArgs{
		user:       *user,
		gateway:    *gateway,
		ip:         *ip,
		workDir:    *workDir,
		privileged: *privileged,
		tty:        *tty,
		openStdin:  *openStdin,
		env:        env,
		args:       flag.Args(),
		mtu:        *mtu,
	}

	if err = dockerInitApp(args); err != nil {
		log.Fatal(err)
	}
}
