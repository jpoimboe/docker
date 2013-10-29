package libvirt

/*
#cgo LDFLAGS: -lvirt
#include <stdlib.h>
#include <libvirt/libvirt.h>
*/
import "C"

import (
	"io/ioutil"
	"os"
	"unsafe"
)

type LibvirtNetworkPlugin struct{}

func NewNetworkPlugin() (*LibvirtNetworkPlugin, error) {
	// Create the plugin struct and test libvirtd connection
	plugin := new(LibvirtNetworkPlugin)
	conn, err := connect()
	if err != nil {
		return nil, err
	}
	defer C.virConnectClose(conn)

	return plugin, nil
}

type TemplateData struct {
	Name       string
	BridgeName string
	Address    string
}

const (
	DefaultNetworkName = "docker"
	DefaultBridgeName  = "docker-lv0"
)

func (plugin *LibvirtNetworkPlugin) DefaultBridge() string {
	return DefaultBridgeName
}

func (plugin *LibvirtNetworkPlugin) CreateBridge(bridge, address string) error {

	conn, err := connect()
	if err != nil {
		return err
	}
	defer C.virConnectClose(conn)

	// Remove any previous definition or creation of the network so we're
	// starting with a clean slate
	name := C.CString(DefaultNetworkName)
	defer C.free(unsafe.Pointer(name))
	network := C.virNetworkLookupByName(conn, name)
	if network != nil {
		defer C.virNetworkFree(network)
		if C.virNetworkIsActive(network) == 1 {
			C.virNetworkDestroy(network)
		}
		C.virNetworkUndefine(network)
	}

	// Generate libvirt network XML file
	file, err := ioutil.TempFile("/tmp", "docker-libvirt")
	if err != nil {
		return err
	}
	filename := file.Name()
	defer os.Remove(filename)
	templateData := &TemplateData{
		Name:       DefaultNetworkName,
		BridgeName: bridge,
		Address:    address,
	}
	err = LibvirtNetworkTemplateCompiled.Execute(file, templateData)
	if err != nil {
		file.Close()
		return err
	}
	file.Close()

	// Define network
	// Currently we use a persistent network because the RHEL 6 version of
	// libvirt can't deal with a temporary network on libvirtd restart.
	buf, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}
	xml := C.CString(string(buf))
	defer C.free(unsafe.Pointer(xml))
	network = C.virNetworkDefineXML(conn, xml)
	if network == nil {
		return libvirtError("virNetworkDefineXML")
	}
	defer C.virNetworkFree(network)
	if C.virNetworkSetAutostart(network, 0) == -1 {
		return libvirtError("virNetworkSetAutostart")
	}

	// Create the bridge
	if C.virNetworkCreate(network) == -1 {
		return libvirtError("virNetworkCreate")
	}

	return nil
}
