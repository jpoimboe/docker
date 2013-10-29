package libvirt

import (
	"html/template"
)

const LibvirtLxcTemplate = `
<domain type='lxc'>
  <name>{{.ID}}</name>
{{with .Memory}}
  <memory unit='b'>{{.}}</memory>
{{else}}
  <memory>500000</memory>
{{end}}
  <os>
    <type>exe</type>
    <init>{{.Cmd}}</init>
{{range .Params}}
    <initarg>{{.}}</initarg>
{{end}}
  </os>
  <vcpu>1</vcpu>
{{with .CpuShares}}
  <cputune>
    <shares>{{.}}</shares>
  </cputune>
{{end}}
{{if .Memory}}
  <memtune>
    <hard_limit unit='bytes'>{{.Memory}}</hard_limit>
    <soft_limit unit='bytes'>{{.Memory}}</soft_limit>
{{with $memSwap := .MemorySwap}}
    <swap_hard_limit unit='bytes'>{{$memSwap}}</swap_hard_limit>
{{end}}
  </memtune>
{{end}}
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>destroy</on_crash>
  <clock offset='utc'/>
  <devices>
    <emulator>/usr/libexec/libvirt_lxc</emulator>
    <filesystem type='mount'>
      <source dir='{{.RootfsPath}}'/>
      <target dir='/'/>
    </filesystem>
    <filesystem type='mount'>
      <source dir='{{.SharedPath}}'/>
      <target dir='/.docker-shared'/>
    </filesystem>
    <filesystem type='mount'>
      <source dir='{{.SysInitPath}}'/>
      <target dir='/.dockerinit'/>
      <readonly/>
    </filesystem>
    <filesystem type='mount'>
      <source dir='{{.EnvConfigPath}}'/>
      <target dir='/.dockerenv'/>
      <readonly/>
    </filesystem>
    <filesystem type='mount'>
      <source dir='{{.ResolvConfPath}}'/>
      <target dir='/etc/resolv.conf'/>
      <readonly/>
    </filesystem>
{{with .HostnamePath}}
    <filesystem type='mount'>
      <source dir='{{.}}'/>
      <target dir='/etc/hostname'/>
      <readonly/>
    </filesystem>
{{end}}
{{with .HostsPath}}
    <filesystem type='mount'>
      <source dir='{{.}}'/>
      <target dir='/etc/hosts'/>
      <readonly/>
    </filesystem>
{{end}}
{{if .Volumes}}
{{ $rw := .VolumesRW }}
{{range $virtualPath, $realPath := .Volumes}}
    <filesystem type='mount'>
      <source dir='{{$realPath}}'/>
      <target dir='{{$virtualPath}}'/>
{{if index $rw $virtualPath }}
{{else}}
      <readonly/>
{{end}}
    </filesystem>
{{end}}
{{end}}
{{if .NetworkDisabled}}
{{else}}
    <interface type='network'>
      <source network='docker'/>
    </interface>
{{end}}
    <console type='pty'/>
  </devices>
{{if .NetworkDisabled}}
  <features>
    <privnet/>
  </features>
{{end}}
</domain>

`

var LibvirtLxcTemplateCompiled *template.Template

func init() {
	var err error
	funcMap := template.FuncMap{}
	LibvirtLxcTemplateCompiled, err = template.New("libvirt-lxc").Funcs(funcMap).Parse(LibvirtLxcTemplate)
	if err != nil {
		panic(err)
	}
}
