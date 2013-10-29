package libvirt

import (
	"fmt"
	"strings"
	"text/template"
)

const LibvirtNetworkTemplate = `
<network>
  <name>{{.Name}}</name>
  <bridge name="{{.BridgeName}}"/>
  <ip address="{{cidrToIP .Address}}" prefix="{{cidrToPrefix .Address}}"></ip>
</network>
`

var LibvirtNetworkTemplateCompiled *template.Template

func cidrSplit(cidr string) []string {
	split := strings.Split(cidr, "/")
	if len(split) != 2 {
		panic(fmt.Errorf("Bad cidr string %v", cidr))
	}
	return split
}

func cidrToIP(cidr string) string {
	return cidrSplit(cidr)[0]
}

func cidrToPrefix(cidr string) string {
	return cidrSplit(cidr)[1]
}

func init() {
	var err error
	funcMap := template.FuncMap{
		"cidrToIP":     cidrToIP,
		"cidrToPrefix": cidrToPrefix,
	}
	LibvirtNetworkTemplateCompiled, err = template.New("libvirt-network").Funcs(funcMap).Parse(LibvirtNetworkTemplate)
	if err != nil {
		panic(err)
	}
}
