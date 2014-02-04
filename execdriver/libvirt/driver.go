// +build linux,dynbinary
// +build !dockerinit

package libvirt

import (
	"errors"
	"fmt"
	"github.com/dotcloud/docker/execdriver"
	"github.com/dotcloud/docker/pkg/cgroups"
	"github.com/dotcloud/docker/pkg/libvirt"
	"github.com/dotcloud/docker/pkg/rpcfd"
	"github.com/dotcloud/docker/utils"
	"html/template"
	"io"
	"io/ioutil"
	"net"
	"net/rpc"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type libvirtConfig struct {
	ID string

	Cmd    string
	Params []string

	RootfsPath string

	NetworkDisabled bool
	Privileged      bool
	Unconfined      bool

	Bridge string

	Memory     int64
	MemorySwap int64
	CpuShares  int64
}

type driver struct {
	root     string // root path for the driver to use
	version  string
	template *template.Template
}

type dockerInit struct {
	command *execdriver.Command
	socket  *net.UnixConn // needed to prevent rpc FD leak bug
	rpc     *rpc.Client
	rpcLock chan struct{}
}

func (init *dockerInit) Call(method string, args, reply interface{}) error {
	select {
	case <-init.rpcLock:
	case <-time.After(time.Second):
		close(init.rpcLock)
		return fmt.Errorf("timeout waiting for rpc connection")
	}

	if init.rpc == nil {
		return fmt.Errorf("no rpc connection to container")
	}

	if err := init.rpc.Call("DockerInit."+method, args, reply); err != nil {
		return fmt.Errorf("dockerinit rpc call %s failed: %s", method, err)
	}
	return nil
}

func (init *dockerInit) GetState() (*DockerInitState, error) {
	var state DockerInitState
	var dummy1 int
	if err := init.Call("GetState", &dummy1, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (init *dockerInit) GetPid() (int, error) {
	var pid rpcfd.RpcPid
	var dummy1 int
	if err := init.Call("GetPid", &dummy1, &pid); err != nil {
		return -1, err
	}
	return int(pid.Pid), nil
}

func (init *dockerInit) Resume() error {
	var dummy1, dummy2 int
	if err := init.Call("Resume", &dummy1, &dummy2); err != nil {
		return err
	}
	return nil
}

func (init *dockerInit) WaitForStateChange(current State) (*DockerInitState, error) {
	var state DockerInitState
	if err := init.Call("WaitForStateChange", &current, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func getFdFromReader(r io.Reader) (*os.File, bool, error) {
	// If already a fd, return it
	if f, ok := r.(*os.File); ok {
		return f, false, nil
	}

	// Otherwise, return a pipe that we copy to the reader
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, false, err
	}

	go func() {
		io.Copy(pw, r)
		pw.Close()
	}()

	return pr, true, nil
}

func (init *dockerInit) SetStdin(r io.Reader) error {
	f, doClose, err := getFdFromReader(r)
	if err != nil {
		return err
	}
	if doClose {
		defer f.Close()
	}

	fd := rpcfd.RpcFd{
		Fd: f.Fd(),
	}

	var dummy int
	if err := init.Call("SetStdin", &fd, &dummy); err != nil {
		return err
	}
	return nil
}

func getFdFromWriter(w io.Writer) (*os.File, bool, error) {
	// If already a fd, return it
	if f, ok := w.(*os.File); ok {
		return f, false, nil
	}

	// Otherwise, return a pipe that we copy to the writer
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, false, err
	}

	go func() {
		io.Copy(w, pr)
		pr.Close()
	}()

	return pw, true, nil
}

func (init *dockerInit) SetWriter(method string, w io.Writer) error {
	f, doClose, err := getFdFromWriter(w)
	if err != nil {
		return err
	}
	if doClose {
		defer f.Close()
	}

	fd := rpcfd.RpcFd{
		Fd: f.Fd(),
	}

	var dummy int
	if err := init.Call(method, &fd, &dummy); err != nil {
		return err
	}
	return nil
}

func (init *dockerInit) SetStdout(w io.Writer) error {
	return init.SetWriter("SetStdout", w)
}

func (init *dockerInit) SetStderr(w io.Writer) error {
	return init.SetWriter("SetStderr", w)
}

func (init *dockerInit) GetStdin() (io.Reader, error) {
	var fd rpcfd.RpcFd
	var dummy int
	if err := init.Call("GetStdin", &dummy, &fd); err != nil {
		return nil, err
	}
	return os.NewFile(fd.Fd, "stdin"), nil
}

func (init *dockerInit) GetStdout() (io.Writer, error) {
	var fd rpcfd.RpcFd
	var dummy int
	if err := init.Call("GetStdout", &dummy, &fd); err != nil {
		return nil, err
	}
	return os.NewFile(fd.Fd, "stdout"), nil
}

func (init *dockerInit) GetStderr() (io.Writer, error) {
	var fd rpcfd.RpcFd
	var dummy int
	if err := init.Call("GetStderr", &dummy, &fd); err != nil {
		return nil, err
	}
	return os.NewFile(fd.Fd, "stderr"), nil
}

func (init *dockerInit) Signal(signal syscall.Signal) error {
	var dummy1 int
	if err := init.Call("Signal", &signal, &dummy1); err != nil {
		return err
	}
	return nil
}

func (init *dockerInit) close() {
	if init.rpc != nil {
		if err := init.rpc.Close(); err != nil {
			// FIXME: Prevent an FD leak by closing the socket
			// directly.  Due to a Go bug, rpc client Close()
			// returns an error if the connection has closed on the
			// other end, and doesn't close the actual socket FD.
			//
			// https://code.google.com/p/go/issues/detail?id=6897
			//
			if err := init.socket.Close(); err != nil {
				utils.Errorf("%s: Error closing RPC socket: %s", init.command.ID, err)
			}
		}
		init.rpc = nil
		init.socket = nil
	}
}

func (init *dockerInit) wait(c *execdriver.Command, startCallback execdriver.StartCallback, reconnect bool) (int, error) {
	state, err := init.GetState()
	if err != nil {
		return -1, err
	}

	consoleConnected := false

	for {
		switch state.State {
		case Initial:
			if c.Stdin != nil {
				if err := init.SetStdin(c.Stdin); err != nil {
					return -1, err
				}
			}
			if err := init.SetStdout(c.Stdout); err != nil {
				return -1, err
			}
			if err := init.SetStderr(c.Stderr); err != nil {
				return -1, err
			}
			consoleConnected = true

			if err := init.Resume(); err != nil {
				return -1, err
			}

		case Running:
			if reconnect && !consoleConnected {
				c.Stdin, err = init.GetStdin()
				if err != nil {
					return -1, err
				}
				c.Stdout, err = init.GetStdout()
				if err != nil {
					return -1, err
				}
				c.Stderr, err = init.GetStderr()
				if err != nil {
					return -1, err
				}
				// FIXME: fix console, logging
				consoleConnected = true
			}
			if startCallback != nil {
				startCallback(c)
			}
			// Just, wait!
			// TODO: Need initlock?

		case Exited:
			// Tell dockerinit it can die
			init.Resume()

			return state.ExitCode, nil

		case FailedToStart:
			// Tell dockerinit it can die
			init.Resume()

			return -1, errors.New(state.Error)

		default:
			return -1, fmt.Errorf("Container is in an unknown state")
		}

		state, err = init.WaitForStateChange(state.State)
		if err != nil {
			return -1, err
		}
	}

	panic("Unreachable")
}

// Connect to the dockerinit RPC socket
func connectToDockerInit(c *execdriver.Command, reconnect bool) (*dockerInit, error) {
	// We can't connect to the dockerinit RPC socket file directly because
	// the path to it is longer than 108 characters (UNIX_PATH_MAX).
	// Create a temporary symlink to connect to.
	// TODO: Make random temp and safe
	symlink := "/tmp/docker-rpc." + c.ID
	os.Symlink(path.Join(c.Rootfs, SocketPath, RpcSocketName), symlink)
	defer os.Remove(symlink)
	address, err := net.ResolveUnixAddr("unix", symlink)
	if err != nil {
		return nil, err
	}

	init := &dockerInit{
		command: c,
		rpcLock: make(chan struct{}),
	}

	// Connect to the dockerinit RPC socket with a 1 second timeout
	for startTime := time.Now(); time.Since(startTime) < time.Second; time.Sleep(10 * time.Millisecond) {
		if init.socket, err = net.DialUnix("unix", nil, address); err == nil {
			init.rpc = rpcfd.NewClient(init.socket)
			break
		}

		if reconnect {
			return nil, fmt.Errorf("container is no longer running")
		}
	}

	if err != nil {
		return nil, fmt.Errorf("socket connection failed: %s", err)
	}

	close(init.rpcLock)

	pid, err := init.GetPid()
	if err != nil {
		return nil, err
	}

	c.Process, err = os.FindProcess(pid)
	if err != nil {
		return nil, err
	}

	return init, nil
}

func NewDriver(root string) (*driver, error) {
	// test libvirtd connection
	conn, err := libvirt.Connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	template, err := getTemplate()
	if err != nil {
		return nil, err
	}

	return &driver{
		root:     root,
		version:  conn.Version(),
		template: template,
	}, nil
}

// Workaround for upstream libvirt bug (error on > 60 chars)
// This is a temporary fix for https://bugzilla.redhat.com/show_bug.cgi?id=1033369
func truncateID(id string) string {
	return id[0:10]
}

func (d *driver) Name() string {
	return fmt.Sprintf("%s-%s", DriverName, d.version)
}

func (d *driver) Run(c *execdriver.Command, startCallback execdriver.StartCallback) (int, error) {
	params := []string{
		"-driver",
		DriverName,
	}

	if c.Network != nil {
		params = append(params,
			"-g", c.Network.Gateway,
			"-i", fmt.Sprintf("%s/%d", c.Network.IPAddress, c.Network.IPPrefixLen),
			"-mtu", strconv.Itoa(c.Network.Mtu),
		)
	}

	if c.User != "" {
		params = append(params, "-u", c.User)
	}

	if c.Privileged {
		params = append(params, "-privileged")
	}

	if c.WorkingDir != "" {
		params = append(params, "-w", c.WorkingDir)
	}

	params = append(params, "--", c.Entrypoint)
	params = append(params, c.Arguments...)

	config := &libvirtConfig{
		ID:              truncateID(c.ID),
		Cmd:             c.InitPath,
		Params:          params,
		Memory:          c.Resources.Memory,
		MemorySwap:      c.Resources.MemorySwap,
		CpuShares:       c.Resources.CpuShares,
		RootfsPath:      c.Rootfs,
		Privileged:      c.Privileged,
		NetworkDisabled: c.Network == nil,
	}

	// Connect to libvirtd
	conn, err := libvirt.Connect()
	if err != nil {
		return -1, err
	}
	defer conn.Close()

	// Generate libvirt domain XML file
	filename := path.Join(d.root, "containers", c.ID, "libvirt-lxc-config.xml")
	file, err := os.Create(filename)
	if err != nil {
		return -1, err
	}

	if err = d.template.Execute(file, config); err != nil {
		file.Close()
		return -1, err
	}
	file.Close()

	// Start up the container
	buf, err := ioutil.ReadFile(filename)
	if err != nil {
		return -1, err
	}
	domain, err := conn.DomainCreateXML(string(buf))
	if err != nil {
		return -1, err
	}
	defer domain.Free()

	init, err := connectToDockerInit(c, false)
	if err != nil {
		return -1, err
	}
	defer init.close()

	return init.wait(c, startCallback, false)
}

func (d *driver) Kill(c *execdriver.Command, sig int) error {
	utils.Debugf("%v: killing container", c.ID)

	id := truncateID(c.ID)

	conn, err := libvirt.Connect()
	if err != nil {
		return err
	}
	defer conn.Close()

	domain, err := conn.DomainLookupByName(id)
	if err != nil {
		return err
	}
	defer domain.Free()

	return domain.Destroy()
}

func (d *driver) Restore(c *execdriver.Command) (int, error) {
	init, err := connectToDockerInit(c, true)
	if err != nil {
		return -1, err
	}
	defer init.close()

	return init.wait(c, nil, true)
}

type info struct {
	ID     string
	driver *driver
}

func (i *info) IsRunning() bool {
	id := truncateID(i.ID)

	conn, err := libvirt.Connect()
	if err != nil {
		return false
	}
	defer conn.Close()

	domain, err := conn.DomainLookupByName(id)
	if domain == nil {
		return false
	}

	domain.Free()

	return true
}

func (d *driver) Info(id string) execdriver.Info {
	return &info{
		ID:     id,
		driver: d,
	}
}

func (d *driver) GetPidsForContainer(id string) ([]int, error) {

	id = truncateID(id)

	conn, err := libvirt.Connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	domain, err := conn.DomainLookupByName(id)
	if domain == nil {
		return nil, err
	}

	defer domain.Free()

	libvirtPid, err := domain.GetId()
	if err != nil {
		return nil, err
	}

	// Get libvirt_lxc's cgroup
	subsystem := "memory"
	cgroupRoot, err := cgroups.FindCgroupMountpoint(subsystem)
	if err != nil {
		return nil, err
	}
	cgroupFile := filepath.Join("/proc", strconv.Itoa(int(libvirtPid)), "cgroup")
	f, err := os.Open(cgroupFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cgroup, err := cgroups.ParseCgroupFile(subsystem, f)
	if err != nil {
		return nil, err
	}

	// Get other pids in cgroup
	tasksFile := filepath.Join(cgroupRoot, cgroup, "tasks")
	output, err := ioutil.ReadFile(tasksFile)
	if err != nil {
		return nil, err
	}
	pids := []int{}
	for _, p := range strings.Split(string(output), "\n") {
		if len(p) == 0 {
			continue
		}
		pid, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("Invalid pid '%s': %s", p, err)
		}
		// skip libvirt_lxc
		if pid == int(libvirtPid) {
			continue
		}

		// TODO: Skip dockerinit for non-machine containers
		pids = append(pids, pid)
	}
	return pids, nil
}
