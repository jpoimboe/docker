package sysinit

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/dotcloud/docker/pkg/netlink"
	"github.com/dotcloud/docker/utils"
	"github.com/syndtr/gocapability/capability"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
)

type DockerInitArgs struct {
	user       string
	gateway    string
	ip         string
	workDir    string
	privileged bool
	env        []string
	args       []string
	mtu        int
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
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

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
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}

	// Start the app
	if err := cmd.Start(); err != nil {
		return err
	}

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

	os.Exit(wstatus.ExitStatus())
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
		env:        env,
		args:       flag.Args(),
		mtu:        *mtu,
	}

	if err = dockerInitApp(args); err != nil {
		log.Fatal(err)
	}
}
