package plugin

import (
	"github.com/dotcloud/docker/utils"
)

type ContainerPlugin interface {
	Version() string
	Start(config *ContainerConfig) error
	Kill(id string) error
	IsRunning(id string) (bool, error)
	Processes(id string) ([]int, error)
}

type ContainerConfig struct {
	ID string

	Cmd    string
	Params []string

	LxcConf []utils.KeyValuePair

	SysInitPath    string
	ResolvConfPath string
	RootPath       string
	HostnamePath   string
	HostsPath      string
	SharedPath     string
	RootfsPath     string
	EnvConfigPath  string

	Stdout *utils.WriteBroadcaster
	Stderr *utils.WriteBroadcaster

	NetworkDisabled bool
	Privileged      bool
	Unconfined      bool

	Bridge string

	Volumes   map[string]string
	VolumesRW map[string]bool

	Memory     int64
	MemorySwap int64
	CpuShares  int64
}
