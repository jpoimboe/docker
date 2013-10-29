package dockernet

import (
	"fmt"
	"github.com/dotcloud/docker/netlink"
	"net"
	"syscall"
	"unsafe"
)

const siocBRADDBR = 0x89a0

type DockerNetworkPlugin struct {
}

func NewNetworkPlugin() (*DockerNetworkPlugin, error) {
	return new(DockerNetworkPlugin), nil
}

const DefaultBridgeName = "docker0"

func (plugin *DockerNetworkPlugin) DefaultBridge() string {
	return DefaultBridgeName
}

func (plugin *DockerNetworkPlugin) CreateBridge(bridge, address string) error {

	if err := createBridgeIface(bridge); err != nil {
		return err
	}

	iface, err := net.InterfaceByName(bridge)
	if err != nil {
		return err
	}
	ipAddr, ipNet, err := net.ParseCIDR(address)
	if err != nil {
		return err
	}
	if netlink.NetworkLinkAddIp(iface, ipAddr, ipNet); err != nil {
		return fmt.Errorf("Unable to add private network: %s", err)
	}

	if err := netlink.NetworkLinkUp(iface); err != nil {
		return fmt.Errorf("Unable to start network bridge: %s", err)
	}

	return nil
}

// Create the actual bridge device.  This is more backward-compatible than
// netlink.NetworkLinkAdd and works on RHEL 6.
func createBridgeIface(name string) error {
	s, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, syscall.IPPROTO_IP)
	if err != nil {
		s, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_IP)
		if err != nil {
			return fmt.Errorf("Error creating bridge creation socket: %s", err)
		}
	}
	defer syscall.Close(s)

	nameBytePtr, err := syscall.BytePtrFromString(name)
	if err != nil {
		return fmt.Errorf("Error converting bridge name %s to byte array: %s", name, err)
	}

	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(s), siocBRADDBR, uintptr(unsafe.Pointer(nameBytePtr))); err != 0 {
		return fmt.Errorf("Error creating bridge: %s", err)
	}
	return nil
}

