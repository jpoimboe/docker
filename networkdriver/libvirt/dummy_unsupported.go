// Dummy file to include if not otherwise building libvirt driver
// Include on non-Linux, or if static binary (libvirt doesn't like static linking)
// +build !linux !dynbinary
package libvirt
