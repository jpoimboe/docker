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

static size_t size_of_long()
{
	return sizeof(long);
}

static unsigned long ptr_to_long(unsigned long *ptr)
{
	return *ptr;
}
*/
import "C"

import (
	"fmt"
	"github.com/dotcloud/docker/plugin"
	"github.com/dotcloud/docker/utils"
	"io"
	"io/ioutil"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

type LibvirtContainerPlugin struct{}

func NewContainerPlugin() (*LibvirtContainerPlugin, error) {

	utils.Debugf("NewContainerPlugin")

	// Register a no-op error handling function with libvirt so that it
	// won't print to stderr
	C.virSetErrorFunc(nil, C.vir_error_func_ptr())

	// Create the plugin struct and test libvirtd connection
	plugin := new(LibvirtContainerPlugin)
	conn, err := connect()
	if err != nil {
		return nil, err
	}
	defer C.virConnectClose(conn)

	return plugin, nil
}

func (plugin *LibvirtContainerPlugin) Version() string {
	conn, err := connect()
	if err != nil {
		return fmt.Sprintf("can't connect to libvirtd (%s)", err)
	}
	defer C.virConnectClose(conn)
	longSize := C.size_of_long()
	versionPtr := C.malloc(longSize)
	defer C.free(versionPtr)
	ret := C.virConnectGetLibVersion(conn, (*_Ctype_ulong)(versionPtr))
	var versionStr string
	if ret == -1 {
		versionStr = "unknown"
	} else {
		version := C.ptr_to_long((*_Ctype_ulong)(versionPtr))
		major := version / 1000000
		version = version % 1000000
		minor := version / 1000
		rel := version % 1000
		versionStr = fmt.Sprintf("%d.%d.%d", major, minor, rel)
	}
	return "libvirt " + versionStr
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

func connect() (C.virConnectPtr, error) {
	uri := C.CString("lxc:///")
	defer C.free(unsafe.Pointer(uri))
	conn := C.virConnectOpenAuth(uri, C.virConnectAuthPtrDefault, 0)
	if conn == nil {
		return nil, libvirtError("virConnectOpenAuth")
	}
	return conn, nil
}

func (_ *LibvirtContainerPlugin) Start(config *plugin.ContainerConfig) error {

	utils.Debugf("%v: starting container", config.ID)

	// Connect to libvirtd
	conn, err := connect()
	if err != nil {
		return err
	}
	defer C.virConnectClose(conn)

	// Generate libvirt domain XML file
	filename := path.Join(config.RootPath, "libvirt-lxc-config.xml")
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	err = LibvirtLxcTemplateCompiled.Execute(file, config)
	if err != nil {
		file.Close()
		return err
	}
	file.Close()

	// Start up the container
	buf, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}
	xml := C.CString(string(buf))
	defer C.free(unsafe.Pointer(xml))
	domain := C.virDomainCreateXML(conn, xml, 0)
	if domain == nil {
		return libvirtError("virDomainCreateXML")
	}
	defer C.virDomainFree(domain)

	// Hook up stdout and stderr so that any early error output that might
	// occur (before dockerinit can hook up the console FDs and pause) will
	// hopefully get logged.  Note that the container has already been
	// started so there's still a small window of time here where we could
	// miss some console output if there's a very early error.
	//
	// We figure out the pty slave device by (crudely) parsing the
	// container's XML.  This is the only known reliable way to hook into
	// the container's console from libvirt.
	//
	// (The virStream interfaces didn't work.  And even if they did, they'd
	// require going through libvirt which we want to avoid.)
	xml = C.virDomainGetXMLDesc(domain, 0)
	if xml == nil {
		return libvirtError("virDomainGetXMLDesc")
	}
	defer C.free(unsafe.Pointer(xml))

	re, err := regexp.Compile("<console type='pty' tty='(.*)'")
	if err != nil {
		return err
	}
	matches := re.FindStringSubmatch(C.GoString(xml))
	if matches == nil || len(matches) != 2 {
		return fmt.Errorf("can't find console element in libvirt domain XML")
	}
	ptyName := matches[1]
	prefix := "/dev/pts/"
	if len(ptyName) <= len(prefix) || ptyName[:len(prefix)] != prefix {
		return fmt.Errorf("non-pts device %v in libvirt domain XML", ptyName)
	}

	// Hook the pty slave up to docker.  This is actually a pty slave into
	// libvirt_lxc, which routes it through another pty master/slave pair
	// to the container's console.
	pty, err := os.OpenFile(ptyName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return err
	}
	// Copy pty output to docker's stderr broadcaster, since any early
	// output coming from libvirt_lxc or dockerinit before getting the
	// proper console FDs hooked up would be an error.
	go func() {
		io.Copy(config.Stderr, pty)
		pty.Close()
	}()

	return nil
}

func (_ *LibvirtContainerPlugin) Kill(id string) error {

	utils.Debugf("%v: killing container", id)

	conn, err := connect()
	if err != nil {
		return err
	}
	defer C.virConnectClose(conn)

	c_id := C.CString(id)
	defer C.free(unsafe.Pointer(c_id))
	domain := C.virDomainLookupByName(conn, c_id)
	if domain == nil {
		return libvirtError("virDomainLookupByName")
	}
	defer C.virDomainFree(domain)

	ret := C.virDomainDestroy(domain)
	if ret == -1 {
		return libvirtError("virDomainDestroy")
	}

	return nil
}

func (_ *LibvirtContainerPlugin) IsRunning(id string) (bool, error) {

	conn, err := connect()
	if err != nil {
		return false, err
	}
	defer C.virConnectClose(conn)

	c_id := C.CString(id)
	defer C.free(unsafe.Pointer(c_id))
	domain := C.virDomainLookupByName(conn, c_id)
	if domain == nil {
		return false, nil
	}
	C.virDomainFree(domain)

	return true, nil
}

func (_ *LibvirtContainerPlugin) Processes(id string) ([]int, error) {
	file := "/sys/fs/cgroup/cpu/machine/" + id + ".libvirt-lxc/tasks"
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	tasks := []int{}
	for _, line := range strings.Split(string(data), "\n") {
		task, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}
