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

type NetworkPlugin interface {
	CreateBridge(bridge, address string) error
	DefaultBridge() string
}

type ContainerConfig struct {
	ID string

	Cmd    string
	Params []string

	LxcConf []utils.KeyValuePair

	RootPath   string
	RootfsPath string

	Stdout *utils.WriteBroadcaster
	Stderr *utils.WriteBroadcaster

	NetworkDisabled bool
	Privileged      bool
	Unconfined      bool

	Bridge string

	Memory     int64
	MemorySwap int64
	CpuShares  int64
}
