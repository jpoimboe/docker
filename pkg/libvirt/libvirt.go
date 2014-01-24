// +build linux

package libvirt

/*
#cgo LDFLAGS: -lvirt
#include <stdlib.h>
#include <libvirt/libvirt.h>
#include <libvirt/virterror.h>
#include <string.h>

static void vir_error_func(void *userData, virErrorPtr error)
{

}

static virErrorFunc vir_error_func_ptr() { return vir_error_func; }

*/
import "C"

import (
	"fmt"
	"unsafe"
)

type Connection struct {
	ptr C.virConnectPtr
}

type Domain struct {
	ptr C.virDomainPtr
}

type Network struct {
	ptr C.virNetworkPtr
}

func (dom *Domain) Destroy() error {
	ret := C.virDomainDestroy(dom.ptr)
	if ret == -1 {
		return libvirtError("virDomainDestroy")
	}
	return nil
}

func (dom *Domain) GetId() (uint32, error) {
	libvirtPid := C.virDomainGetID(dom.ptr)
	if C.int(libvirtPid) == -1 {
		return 0, libvirtError("virDomainGetID")
	}
	return uint32(libvirtPid), nil
}

func (dom *Domain) Free() {
	C.virDomainFree(dom.ptr)
}

func init() {
	// Register a no-op error handling function with libvirt so that it
	// won't print to stderr
	C.virSetErrorFunc(nil, C.vir_error_func_ptr())
}

func Connect() (*Connection, error) {
	uri := C.CString("lxc:///")
	defer C.free(unsafe.Pointer(uri))
	conn := C.virConnectOpenAuth(uri, C.virConnectAuthPtrDefault, 0)
	if conn == nil {
		return nil, libvirtError("virConnectOpenAuth")
	}
	return &Connection{ptr: conn}, nil
}

func (conn *Connection) Close() {
	C.virConnectClose(conn.ptr)
}

func (conn *Connection) Version() string {
	var version C.ulong
	ret := C.virConnectGetLibVersion(conn.ptr, &version)
	if ret == -1 {
		return "unknown"
	} else {
		major := version / 1000000
		version = version % 1000000
		minor := version / 1000
		rel := version % 1000
		return fmt.Sprintf("%d.%d.%d", major, minor, rel)
	}
}

func (conn *Connection) DomainCreateXML(xml string) (*Domain, error) {
	xmlC := C.CString(xml)
	defer C.free(unsafe.Pointer(xmlC))
	domain := C.virDomainCreateXML(conn.ptr, xmlC, 0)
	if domain == nil {
		return nil, libvirtError("virDomainCreateXML")
	}
	return &Domain{ptr: domain}, nil
}

func (conn *Connection) DomainLookupByName(name string) (*Domain, error) {
	nameC := C.CString(name)
	defer C.free(unsafe.Pointer(nameC))
	domain := C.virDomainLookupByName(conn.ptr, nameC)
	if domain == nil {
		return nil, libvirtError("virDomainLookupByName")
	}
	return &Domain{ptr: domain}, nil
}

func (conn *Connection) NetworkLookupByName(name string) (*Network, error) {
	nameC := C.CString(name)
	defer C.free(unsafe.Pointer(nameC))
	network := C.virNetworkLookupByName(conn.ptr, nameC)
	if network == nil {
		return nil, libvirtError("virNetworkLookupByName")
	}
	return &Network{ptr: network}, nil
}

func (conn *Connection) NetworkDefineXML(xml string) (*Network, error) {
	xmlC := C.CString(string(xml))
	defer C.free(unsafe.Pointer(xmlC))
	network := C.virNetworkDefineXML(conn.ptr, xmlC)
	if network == nil {
		return nil, libvirtError("virNetworkDefineXML")
	}
	return &Network{ptr: network}, nil
}

func (network *Network) Free() error {
	if ret := C.virNetworkFree(network.ptr); ret == -1 {
		return libvirtError("virNetworkFree")
	}
	return nil
}

func (network *Network) IsActive() (bool, error) {
	ret := C.virNetworkIsActive(network.ptr)
	if ret == -1 {
		return false, libvirtError("virNetworkIsActive")
	}
	return ret == 1, nil
}

func (network *Network) Destroy() error {
	if ret := C.virNetworkDestroy(network.ptr); ret == -1 {
		return libvirtError("virNetworkDestroy")
	}
	return nil
}

func (network *Network) Undefine() error {
	if ret := C.virNetworkUndefine(network.ptr); ret == -1 {
		return libvirtError("virNetworkUndefine")
	}
	return nil
}

func (network *Network) SetAutostart(autostart bool) error {
	var autostartC C.int
	if autostart {
		autostartC = 1
	} else {
		autostartC = 0
	}
	if ret := C.virNetworkSetAutostart(network.ptr, autostartC); ret == -1 {
		return libvirtError("virNetworkSetAutostart")
	}
	return nil
}

func (network *Network) Create() error {
	if ret := C.virNetworkCreate(network.ptr); ret == -1 {
		return libvirtError("virNetworkCreate")
	}
	return nil
}

func (network *Network) GetBridgeName() (string, error) {
	bridgeC := C.virNetworkGetBridgeName(network.ptr)
	if bridgeC == nil {
		return "", libvirtError("virNetworkGetBridgeName")
	}
	defer C.free(unsafe.Pointer(bridgeC))
	return C.GoString(bridgeC), nil
}

func libvirtError(str string) error {
	lastError := C.virGetLastError()

	// There's no virGetLastErrorMessage() in RHEL 6, so implement it here
	// for maximum compatibility.
	var libvirtErrorStr string
	if lastError == nil || lastError.code == C.VIR_ERR_OK {
		libvirtErrorStr = "no error"
	} else if lastError.message == nil {
		libvirtErrorStr = "unknown error"
	} else {
		libvirtErrorStr = C.GoString(lastError.message)
	}

	return fmt.Errorf(str + ": " + libvirtErrorStr)
}
