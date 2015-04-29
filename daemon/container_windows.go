// +build windows

package daemon

import (
	"fmt"
	"strings"

	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/daemon/network"
	"github.com/docker/docker/engine"
	"github.com/docker/docker/pkg/archive"
)

// TODO Windows. A reasonable default at the moment.
const DefaultPathEnv = `c:\windows\system32;c:\windows\system32\WindowsPowerShell\v1.0`

type Container struct {
	CommonContainer

	// Fields below here are platform specific.

	// TODO Windows. Further factoring out of unused fields will be necessary.

	// ---- START OF TEMPORARY DECLARATION ----
	// TODO Windows. Temporarily keeping fields in to assist in compilation
	// of the daemon on Windows without affecting many other files in a single
	// PR, thus making code review significantly harder. These lines will be
	// removed in subsequent PRs.

	AppArmorProfile string

	// Store rw/ro in a separate structure to preserve reverse-compatibility on-disk.
	// Easier than migrating older container configs :)
	VolumesRW map[string]bool

	AppliedVolumesFrom map[string]struct{}
	// ---- END OF TEMPORARY DECLARATION ----

}

func killProcessDirectly(container *Container) error {
	return nil
}

func (container *Container) setupContainerDns() error {
	return nil
}

func (container *Container) updateParentsHosts() error {
	return nil
}

func (container *Container) setupLinkedContainers() ([]string, error) {
	return nil
}

func (container *Container) createDaemonEnvironment(linkedEnv []string) []string {
	return nil
}

func (container *Container) initializeNetworking() error {
	return nil
}

func (container *Container) setupWorkingDirectory() error {
	return nil
}

func (container *Container) verifyDaemonSettings() {
}

func populateCommand(c *Container, env []string) error {
	en := &execdriver.Network{
		Mtu:       c.daemon.config.Mtu,
		Interface: nil,
	}

	parts := strings.SplitN(string(c.hostConfig.NetworkMode), ":", 2)
	switch parts[0] {
	case "none":
	case "bridge", "": // empty string to support existing containers
		if !c.Config.NetworkDisabled {
			network := c.NetworkSettings
			en.Interface = &execdriver.NetworkInterface{
				Bridge:     network.Bridge,
				MacAddress: network.MacAddress,
			}
		}
	case "host", "container":
		return fmt.Errorf("unsupported network mode: %s", c.hostConfig.NetworkMode)
	default:
		return fmt.Errorf("invalid network mode: %s", c.hostConfig.NetworkMode)
	}

	pid := &execdriver.Pid{}

	// TODO Windows. This can probably be factored out.
	pid.HostPid = c.hostConfig.PidMode.IsHost()

	// TODO Windows. Resource controls to be implemented later.
	resources := &execdriver.Resources{}

	// TODO Windows. Further refactoring required (privileged/user)
	processConfig := execdriver.ProcessConfig{
		Privileged: c.hostConfig.Privileged,
		Entrypoint: c.Path,
		Arguments:  c.Args,
		Tty:        c.Config.Tty,
		User:       c.Config.User,
	}

	processConfig.Env = env

	// TODO Windows: Factor out remainder of unused fields.
	c.command = &execdriver.Command{
		ID:             c.ID,
		Rootfs:         c.RootfsPath(),
		ReadonlyRootfs: c.hostConfig.ReadonlyRootfs,
		InitPath:       "/.dockerinit",
		WorkingDir:     c.Config.WorkingDir,
		Network:        en,
		Pid:            pid,
		Resources:      resources,
		CapAdd:         c.hostConfig.CapAdd,
		CapDrop:        c.hostConfig.CapDrop,
		ProcessConfig:  processConfig,
		ProcessLabel:   c.GetProcessLabel(),
		MountLabel:     c.GetMountLabel(),
	}

	return nil
}

// GetSize, return real size, virtual size
func (container *Container) GetSize() (int64, int64) {
	// TODO Windows
	return 0, 0
}

func (container *Container) AllocateNetwork() error {
	mode := container.hostConfig.NetworkMode
	if container.Config.NetworkDisabled || !mode.IsPrivate() {
		return nil
	}

	var (
		env *engine.Env
		err error
		eng = container.daemon.eng
	)

	job := eng.Job("allocate_interface", container.ID)
	job.Setenv("RequestedMac", container.Config.MacAddress)
	if env, err = job.Stdout.AddEnv(); err != nil {
		return err
	}
	if err = job.Run(); err != nil {
		return err
	}

	container.NetworkSettings.Bridge = env.Get("Bridge")
	container.NetworkSettings.MacAddress = env.Get("MacAddress")

	return nil
}

func (container *Container) ExportRw() (archive.Archive, error) {
	if container.IsRunning() {
		return nil, fmt.Errorf("Cannot export a running container.")
	}
	// TODO Windows. Implementation (different to Linux)
	return nil, nil
}
