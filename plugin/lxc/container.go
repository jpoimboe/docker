package lxc

import (
	"fmt"
	"github.com/dotcloud/docker/plugin"
	"github.com/dotcloud/docker/utils"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type LxcContainerPlugin struct{}

func NewContainerPlugin() (*LxcContainerPlugin, error) {
	if _, err := exec.LookPath("lxc-start"); err != nil {
		return nil, fmt.Errorf("lxc-start not found")
	}
	plugin := new(LxcContainerPlugin)
	return plugin, nil
}

func (_ *LxcContainerPlugin) Version() string {
	version := ""
	if output, err := exec.Command("lxc-version").CombinedOutput(); err == nil {
		outputStr := string(output)
		if len(strings.SplitN(outputStr, ":", 2)) == 2 {
			version = strings.TrimSpace(strings.SplitN(string(output), ":", 2)[1])
		} else {
			version = "unknown"
		}
	}
	return "lxc " + version
}

func lxcConfigPath(config *plugin.ContainerConfig) string {
	return path.Join(config.RootPath, "config.lxc")
}

func (_ *LxcContainerPlugin) Start(config *plugin.ContainerConfig) error {

	utils.Debugf("%v: starting container", config.ID)

	// Generate config file
	file, err := os.Create(lxcConfigPath(config))
	if err != nil {
		return err
	}
	defer file.Close()
	if err := LxcTemplateCompiled.Execute(file, config); err != nil {
		return err
	}

	lxcStart := "lxc-start"

	// Symlink lxc-start-unconfined -> lxc-start to avoid AppArmor
	// confinement in privileged mode
	if config.Unconfined {
		utils.Debugf("Escaping AppArmor confinement")

		sourcePath := path.Join(config.RootPath, "lxc-start-unconfined")

		targetPath, err := exec.LookPath("lxc-start")
		if err != nil {
			return err
		}

		os.Remove(sourcePath)
		if err := os.Symlink(targetPath, sourcePath); err != nil {
			return err
		}

		lxcStart = sourcePath
	}

	// Assemble lxc-start parameters
	lxcParams := []string{
		lxcStart,
		"-n", config.ID,
		"-f", lxcConfigPath(config),
		"--",
		config.Cmd,
	}
	lxcParams = append(lxcParams, config.Params...)
	isShared, err := rootIsShared()
	if err != nil {
		return err
	}
	if isShared {
		// lxc-start really needs / to be non-shared, or all kinds of stuff break
		// when lxc-start unmount things and those unmounts propagate to the main
		// mount namespace.
		// What we really want is to clone into a new namespace and then
		// mount / MS_REC|MS_SLAVE, but since we can't really clone or fork
		// without exec in go we have to do this horrible shell hack...
		shellString :=
			"mount --make-rslave /; exec " +
				utils.ShellQuoteArguments(lxcParams)

		lxcParams = []string{
			"unshare", "-m", "--", "/bin/sh", "-c", shellString,
		}
	}
	cmd := exec.Command(lxcParams[0], lxcParams[1:]...)

	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Hook up stdout and stderr to the cmd so that any early error output
	// that might occur (before hooking up the console FDs) gets logged
	cmd.Stdout = config.Stdout
	cmd.Stderr = config.Stderr

	// Start it
	return cmd.Start()
}

func rootIsShared() (bool, error) {
	if data, err := ioutil.ReadFile("/proc/self/mountinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			cols := strings.Split(line, " ")
			if len(cols) >= 6 && cols[4] == "/" {
				return strings.HasPrefix(cols[6], "shared"), nil
			}
		}
	}

	return false, fmt.Errorf("Can't find root mount in /proc/self/mountinfo")
}

func (_ *LxcContainerPlugin) Kill(id string) error {

	utils.Debugf("Kill")

	if err := exec.Command("lxc-shutdown", "-n", id, "-t", "1").Run(); err != nil {
		return err
	}

	return nil
}

func (_ *LxcContainerPlugin) IsRunning(id string) (bool, error) {

	output, err := exec.Command("lxc-info", "-n", id).CombinedOutput()
	if err != nil {
		return false, err
	}
	return strings.Contains(string(output), "RUNNING"), nil
}

func (_ *LxcContainerPlugin) Processes(id string) ([]int, error) {
	pids := []int{}

	// memory is chosen randomly, any cgroup used by docker works
	subsystem := "memory"

	cgroupRoot, err := utils.FindCgroupMountpoint(subsystem)
	if err != nil {
		return pids, err
	}

	cgroupDir, err := getThisCgroup(subsystem)
	if err != nil {
		return pids, err
	}

	filename := filepath.Join(cgroupRoot, cgroupDir, id, "tasks")
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		// With more recent lxc versions use, cgroup will be in lxc/
		filename = filepath.Join(cgroupRoot, cgroupDir, "lxc", id, "tasks")
	}

	output, err := ioutil.ReadFile(filename)
	if err != nil {
		return pids, err
	}
	for _, p := range strings.Split(string(output), "\n") {
		if len(p) == 0 {
			continue
		}
		pid, err := strconv.Atoi(p)
		if err != nil {
			return pids, fmt.Errorf("Invalid pid '%s': %s", p, err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// Returns the relative path to the cgroup docker is running in.
func getThisCgroup(cgroupType string) (string, error) {
	output, err := ioutil.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(output), "\n") {
		parts := strings.Split(line, ":")
		// any type used by docker should work
		if parts[1] == cgroupType {
			return parts[2], nil
		}
	}
	return "", fmt.Errorf("cgroup '%s' not found in /proc/self/cgroup", cgroupType)
}
