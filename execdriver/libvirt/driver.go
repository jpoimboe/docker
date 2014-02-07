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
	case <-time.After(10 * time.Second):
		close(init.rpcLock)
		return fmt.Errorf("timeout waiting for rpc connection")
	}

	if err := init.rpc.Call("DockerInit."+method, args, reply); err != nil {
		return fmt.Errorf("dockerinit rpc call %s failed: %s", method, err)
	}

	return nil
}

func (init *dockerInit) getState() (*StateInfo, error) {
	var stateInfo StateInfo
	var dummy int
	if err := init.Call("GetState", &dummy, &stateInfo); err != nil {
		return nil, err
	}
	return &stateInfo, nil
}

func (init *dockerInit) resume() error {
	var dummy1, dummy2 int
	if err := init.Call("Resume", &dummy1, &dummy2); err != nil {
		return err
	}
	return nil
}

func (init *dockerInit) getPtyMaster() (*os.File, error) {
	var fdRpc rpcfd.RpcFd
	var dummy int
	if err := init.Call("GetPtyMaster", &dummy, &fdRpc); err != nil {
		return nil, err
	}
	return os.NewFile(fdRpc.Fd, "ptyMaster"), nil
}

func (init *dockerInit) getStdin() (*os.File, error) {
	var fdRpc rpcfd.RpcFd
	var dummy int
	if err := init.Call("GetStdin", &dummy, &fdRpc); err != nil {
		return nil, err
	}
	return os.NewFile(fdRpc.Fd, "stdin"), nil
}

func (init *dockerInit) getStdout() (*os.File, error) {
	var fdRpc rpcfd.RpcFd
	var dummy int
	if err := init.Call("GetStdout", &dummy, &fdRpc); err != nil {
		return nil, err
	}
	return os.NewFile(fdRpc.Fd, "stdout"), nil
}

func (init *dockerInit) getStderr() (*os.File, error) {
	var fdRpc rpcfd.RpcFd
	var dummy int
	if err := init.Call("GetStderr", &dummy, &fdRpc); err != nil {
		return nil, err
	}
	return os.NewFile(fdRpc.Fd, "stderr"), nil
}

func (init *dockerInit) getPid() (int, error) {
	var rpcPid rpcfd.RpcPid
	var dummy int
	if err := init.Call("GetPid", &dummy, &rpcPid); err != nil {
		return -1, err
	}
	return int(rpcPid.Pid), nil
}

func (init *dockerInit) signal(signal syscall.Signal) error {
	var dummy int
	if err := init.Call("Signal", &signal, &dummy); err != nil {
		return err
	}
	return nil
}

func (init *dockerInit) connectConsole() error {
	if init.command.Tty {
		ptyMaster, err := init.getPtyMaster()
		if err != nil {
			return err
		}

		if init.command.Stdin != nil {
			go func() {
				io.Copy(ptyMaster, init.command.Stdin)
				ptyMaster.Close()
			}()
		}

		go func() {
			io.Copy(init.command.Stdout, ptyMaster)
			ptyMaster.Close()
		}()
	} else {
		var err error

		stdout, err := init.getStdout()
		if err != nil {
			return err
		}
		go func() {
			io.Copy(init.command.Stdout, stdout)
			stdout.Close()
		}()

		stderr, err := init.getStderr()
		if err != nil {
			return err
		}
		go func() {
			io.Copy(init.command.Stderr, stderr)
			stderr.Close()
		}()

		if init.command.Stdin != nil {
			stdin, err := init.getStdin()
			if err != nil {
				return err
			}
			go func() {
				io.Copy(stdin, init.command.Stdin)
				stdin.Close()
			}()
		}
	}
	return nil
}

func (init *dockerInit) wait(callback execdriver.StartCallback, reconnect bool) (int, error) {

	state, err := init.getState()
	if err != nil {
		return -1, err
	}

	if reconnect {
		switch state.State {
		case Running:
			pid, err := init.getPid()
			if err != nil {
				return -1, err
			}

			init.command.Process, err = os.FindProcess(pid)
			if err != nil {
				return -1, err
			}

			if err := init.connectConsole(); err != nil {
				return -1, err
			}
		default:
			return -1, fmt.Errorf("can't reconnect to container in state %d", state.State)
		}
	}

	for {
		switch state.State {
		case Initial:
			if err := init.resume(); err != nil {
				return -1, err
			}

		case ConsoleReady:
			if err := init.connectConsole(); err != nil {
				return -1, err
			}

			if err := init.resume(); err != nil {
				return -1, err
			}

		case RunReady:
			if err := init.resume(); err != nil {
				return -1, err
			}

		case Running:
			pid, err := init.getPid()
			if err != nil {
				return -1, err
			}

			init.command.Process, err = os.FindProcess(pid)
			if err != nil {
				return -1, err
			}

			if callback != nil {
				callback(init.command)
			}

			if err := init.resume(); err != nil {
				return -1, err
			}

		case Exited:
			// Tell dockerinit it can die, ignore error since the
			// death can disrupt the RPC operation
			init.resume()

			return state.ExitCode, nil

		case FailedToStart:
			// Tell dockerinit it can die, ignore error since the
			// death can disrupt the RPC operation
			init.resume()

			return -1, errors.New(state.Error)

		default:
			return -1, fmt.Errorf("Container is in an unknown state")
		}

		var err error
		state, err = init.getState()
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

	// Connect to the dockerinit RPC socket with a 10 second timeout
	for startTime := time.Now(); time.Since(startTime) < 10*time.Second; time.Sleep(10 * time.Millisecond) {
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

	return init, nil
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

func (d *driver) Run(c *execdriver.Command, callback execdriver.StartCallback) (int, error) {
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

	if c.Tty {
		params = append(params, "-tty")
	}

	if c.Stdin != nil {
		params = append(params, "-openstdin")
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

	return init.wait(callback, false)
}

func (d *driver) Kill(c *execdriver.Command, sig int) error {
	return c.Process.Signal(syscall.Signal(sig))
}

func (d *driver) Restore(c *execdriver.Command) (int, error) {
	init, err := connectToDockerInit(c, true)
	if err != nil {
		return -1, err
	}
	defer init.close()

	return init.wait(nil, true)
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

	// FIXME: ask dockerinit instead and use rpcpid

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

		// The .dockerinit process (pid 1) is an implementation detail,
		// so remove it from the pid list.
		comm, err := ioutil.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
		if err != nil {
			// Ignore any error, the process could have exited
			// already.
			utils.Debugf("can't read comm file for pid %d: %s", pid, err)
			continue
		}
		if strings.TrimSpace(string(comm)) == ".dockerinit" {
			continue
		}

		pids = append(pids, pid)
	}
	return pids, nil
}
