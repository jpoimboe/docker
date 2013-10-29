package dockernet

import (
	"fmt"
	"github.com/dotcloud/docker/netlink"
	"net"
)

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

	err := netlink.NetworkLinkAdd(bridge, "bridge")
	if err != nil {
		return fmt.Errorf("Error creating bridge: %s", err)
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
