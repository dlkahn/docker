package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/docker/libcontainer/label"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/journald"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/docker/docker/daemon/logger/syslog"
	"github.com/docker/docker/daemon/network"
	"github.com/docker/docker/daemon/networkdriver/bridge"
	"github.com/docker/docker/image"
	"github.com/docker/docker/links"
	"github.com/docker/docker/nat"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/broadcastwriter"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/promise"
	"github.com/docker/docker/pkg/symlink"
	"github.com/docker/docker/runconfig"
)

var (
	ErrNotATTY               = errors.New("The PTY is not a file")
	ErrNoTTY                 = errors.New("No PTY found")
	ErrContainerStart        = errors.New("The container failed to start. Unknown error")
	ErrContainerStartTimeout = errors.New("The container failed to start due to timed out.")
)

type StreamConfig struct {
	stdout    *broadcastwriter.BroadcastWriter
	stderr    *broadcastwriter.BroadcastWriter
	stdin     io.ReadCloser
	stdinPipe io.WriteCloser
}

// CommonContainer holds the settings for a container which are applicable
// across all platforms supported by the daemon.
type CommonContainer struct {
	*State `json:"State"` // Needed for remote api version <= 1.11
	root   string         // Path to the "home" of the container, including metadata.
	basefs string         // Path to the graphdriver mountpoint

	ID string

	Created time.Time

	Path string
	Args []string

	Config  *runconfig.Config
	ImageID string `json:"Image"`

	NetworkSettings *network.Settings

	ResolvConfPath string
	HostnamePath   string
	HostsPath      string
	LogPath        string
	Name           string
	Driver         string
	ExecDriver     string

	command *execdriver.Command
	StreamConfig

	daemon                   *Daemon
	MountLabel, ProcessLabel string
	RestartCount             int
	UpdateDns                bool

	// Maps container paths to volume paths.  The key in this is the path to which
	// the volume is being mounted inside the container.  Value is the path of the
	// volume on disk
	Volumes    map[string]string
	hostConfig *runconfig.HostConfig

	activeLinks  map[string]*links.Link
	monitor      *containerMonitor
	execCommands *execStore
	// logDriver for closing
	logDriver logger.Logger
	logCopier *logger.Copier
}

func (container *Container) FromDisk() error {
	pth, err := container.jsonPath()
	if err != nil {
		return err
	}

	jsonSource, err := os.Open(pth)
	if err != nil {
		return err
	}
	defer jsonSource.Close()

	dec := json.NewDecoder(jsonSource)

	// Load container settings
	// udp broke compat of docker.PortMapping, but it's not used when loading a container, we can skip it
	if err := dec.Decode(container); err != nil && !strings.Contains(err.Error(), "docker.PortMapping") {
		return err
	}

	if err := label.ReserveLabel(container.ProcessLabel); err != nil {
		return err
	}
	return container.readHostConfig()
}

func (container *Container) toDisk() error {
	data, err := json.Marshal(container)
	if err != nil {
		return err
	}

	pth, err := container.jsonPath()
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(pth, data, 0666); err != nil {
		return err
	}

	return container.WriteHostConfig()
}

func (container *Container) ToDisk() error {
	container.Lock()
	err := container.toDisk()
	container.Unlock()
	return err
}

func (container *Container) readHostConfig() error {
	container.hostConfig = &runconfig.HostConfig{}
	// If the hostconfig file does not exist, do not read it.
	// (We still have to initialize container.hostConfig,
	// but that's OK, since we just did that above.)
	pth, err := container.hostConfigPath()
	if err != nil {
		return err
	}

	_, err = os.Stat(pth)
	if os.IsNotExist(err) {
		return nil
	}

	f, err := os.Open(pth)
	if err != nil {
		return err
	}
	defer f.Close()

	return json.NewDecoder(f).Decode(&container.hostConfig)
}

func (container *Container) WriteHostConfig() error {
	data, err := json.Marshal(container.hostConfig)
	if err != nil {
		return err
	}

	pth, err := container.hostConfigPath()
	if err != nil {
		return err
	}

	return ioutil.WriteFile(pth, data, 0666)
}

func (container *Container) LogEvent(action string) {
	d := container.daemon
	d.EventsService.Log(
		action,
		container.ID,
		container.Config.Image,
	)
}

// Evaluates `path` in the scope of the container's basefs, with proper path
// sanitisation. Symlinks are all scoped to the basefs of the container, as
// though the container's basefs was `/`.
//
// The basefs of a container is the host-facing path which is bind-mounted as
// `/` inside the container. This method is essentially used to access a
// particular path inside the container as though you were a process in that
// container.
//
// NOTE: The returned path is *only* safely scoped inside the container's basefs
//       if no component of the returned path changes (such as a component
//       symlinking to a different path) between using this method and using the
//       path. See symlink.FollowSymlinkInScope for more details.
func (container *Container) GetResourcePath(path string) (string, error) {
	cleanPath := filepath.Join("/", path)
	return symlink.FollowSymlinkInScope(filepath.Join(container.basefs, cleanPath), container.basefs)
}

// Evaluates `path` in the scope of the container's root, with proper path
// sanitisation. Symlinks are all scoped to the root of the container, as
// though the container's root was `/`.
//
// The root of a container is the host-facing configuration metadata directory.
// Only use this method to safely access the container's `container.json` or
// other metadata files. If in doubt, use container.GetResourcePath.
//
// NOTE: The returned path is *only* safely scoped inside the container's root
//       if no component of the returned path changes (such as a component
//       symlinking to a different path) between using this method and using the
//       path. See symlink.FollowSymlinkInScope for more details.
func (container *Container) GetRootResourcePath(path string) (string, error) {
	cleanPath := filepath.Join("/", path)
	return symlink.FollowSymlinkInScope(filepath.Join(container.root, cleanPath), container.root)
}

func (container *Container) Start() (err error) {
	container.Lock()
	defer container.Unlock()

	if container.Running {
		return nil
	}

	if container.removalInProgress || container.Dead {
		return fmt.Errorf("Container is marked for removal and cannot be started.")
	}

	// if we encounter an error during start we need to ensure that any other
	// setup has been cleaned up properly
	defer func() {
		if err != nil {
			container.setError(err)
			// if no one else has set it, make sure we don't leave it at zero
			if container.ExitCode == 0 {
				container.ExitCode = 128
			}
			container.toDisk()
			container.cleanup()
		}
	}()

	if err := container.setupContainerDns(); err != nil {
		return err
	}
	if err := container.Mount(); err != nil {
		return err
	}
	if err := container.initializeNetworking(); err != nil {
		return err
	}
	if err := container.updateParentsHosts(); err != nil {
		return err
	}
	container.verifyDaemonSettings()
	if err := container.prepareVolumes(); err != nil {
		return err
	}
	linkedEnv, err := container.setupLinkedContainers()
	if err != nil {
		return err
	}
	if err := container.setupWorkingDirectory(); err != nil {
		return err
	}
	env := container.createDaemonEnvironment(linkedEnv)
	if err := populateCommand(container, env); err != nil {
		return err
	}
	if err := container.setupMounts(); err != nil {
		return err
	}

	return container.waitForStart()
}

func (container *Container) Run() error {
	if err := container.Start(); err != nil {
		return err
	}
	container.WaitStop(-1 * time.Second)
	return nil
}

func (container *Container) Output() (output []byte, err error) {
	pipe := container.StdoutPipe()
	defer pipe.Close()
	if err := container.Start(); err != nil {
		return nil, err
	}
	output, err = ioutil.ReadAll(pipe)
	container.WaitStop(-1 * time.Second)
	return output, err
}

// StreamConfig.StdinPipe returns a WriteCloser which can be used to feed data
// to the standard input of the container's active process.
// Container.StdoutPipe and Container.StderrPipe each return a ReadCloser
// which can be used to retrieve the standard output (and error) generated
// by the container's active process. The output (and error) are actually
// copied and delivered to all StdoutPipe and StderrPipe consumers, using
// a kind of "broadcaster".

func (streamConfig *StreamConfig) StdinPipe() io.WriteCloser {
	return streamConfig.stdinPipe
}

func (streamConfig *StreamConfig) StdoutPipe() io.ReadCloser {
	reader, writer := io.Pipe()
	streamConfig.stdout.AddWriter(writer, "")
	return ioutils.NewBufReader(reader)
}

func (streamConfig *StreamConfig) StderrPipe() io.ReadCloser {
	reader, writer := io.Pipe()
	streamConfig.stderr.AddWriter(writer, "")
	return ioutils.NewBufReader(reader)
}

func (streamConfig *StreamConfig) StdoutLogPipe() io.ReadCloser {
	reader, writer := io.Pipe()
	streamConfig.stdout.AddWriter(writer, "stdout")
	return ioutils.NewBufReader(reader)
}

func (streamConfig *StreamConfig) StderrLogPipe() io.ReadCloser {
	reader, writer := io.Pipe()
	streamConfig.stderr.AddWriter(writer, "stderr")
	return ioutils.NewBufReader(reader)
}

func (container *Container) ReleaseNetwork() {
	if container.Config.NetworkDisabled || !container.hostConfig.NetworkMode.IsPrivate() {
		return
	}
	bridge.Release(container.ID)
	container.NetworkSettings = &network.Settings{}
}

// cleanup releases any network resources allocated to the container along with any rules
// around how containers are linked together.  It also unmounts the container's root filesystem.
func (container *Container) cleanup() {
	container.ReleaseNetwork()

	// Disable all active links
	if container.activeLinks != nil {
		for _, link := range container.activeLinks {
			link.Disable()
		}
	}

	if err := container.Unmount(); err != nil {
		logrus.Errorf("%v: Failed to umount filesystem: %v", container.ID, err)
	}

	for _, eConfig := range container.execCommands.s {
		container.daemon.unregisterExecCommand(eConfig)
	}
}

func (container *Container) KillSig(sig int) error {
	logrus.Debugf("Sending %d to %s", sig, container.ID)
	container.Lock()
	defer container.Unlock()

	// We could unpause the container for them rather than returning this error
	if container.Paused {
		return fmt.Errorf("Container %s is paused. Unpause the container before stopping", container.ID)
	}

	if !container.Running {
		return nil
	}

	// signal to the monitor that it should not restart the container
	// after we send the kill signal
	container.monitor.ExitOnNext()

	// if the container is currently restarting we do not need to send the signal
	// to the process.  Telling the monitor that it should exit on it's next event
	// loop is enough
	if container.Restarting {
		return nil
	}

	return container.daemon.Kill(container, sig)
}

// Wrapper aroung KillSig() suppressing "no such process" error.
func (container *Container) killPossiblyDeadProcess(sig int) error {
	err := container.KillSig(sig)
	if err == syscall.ESRCH {
		logrus.Debugf("Cannot kill process (pid=%d) with signal %d: no such process.", container.GetPid(), sig)
		return nil
	}
	return err
}

func (container *Container) Pause() error {
	if container.IsPaused() {
		return fmt.Errorf("Container %s is already paused", container.ID)
	}
	if !container.IsRunning() {
		return fmt.Errorf("Container %s is not running", container.ID)
	}
	return container.daemon.Pause(container)
}

func (container *Container) Unpause() error {
	if !container.IsPaused() {
		return fmt.Errorf("Container %s is not paused", container.ID)
	}
	if !container.IsRunning() {
		return fmt.Errorf("Container %s is not running", container.ID)
	}
	return container.daemon.Unpause(container)
}

func (container *Container) Kill() error {
	if !container.IsRunning() {
		return nil
	}

	// 1. Send SIGKILL
	if err := container.killPossiblyDeadProcess(9); err != nil {
		return err
	}

	// 2. Wait for the process to die, in last resort, try to kill the process directly
	if err := killProcessDirectly(container); err != nil {
		return err
	}

	container.WaitStop(-1 * time.Second)
	return nil
}

func (container *Container) Stop(seconds int) error {
	if !container.IsRunning() {
		return nil
	}

	// 1. Send a SIGTERM
	if err := container.killPossiblyDeadProcess(15); err != nil {
		logrus.Infof("Failed to send SIGTERM to the process, force killing")
		if err := container.killPossiblyDeadProcess(9); err != nil {
			return err
		}
	}

	// 2. Wait for the process to exit on its own
	if _, err := container.WaitStop(time.Duration(seconds) * time.Second); err != nil {
		logrus.Infof("Container %v failed to exit within %d seconds of SIGTERM - using the force", container.ID, seconds)
		// 3. If it doesn't, then send SIGKILL
		if err := container.Kill(); err != nil {
			container.WaitStop(-1 * time.Second)
			return err
		}
	}
	return nil
}

func (container *Container) Restart(seconds int) error {
	// Avoid unnecessarily unmounting and then directly mounting
	// the container when the container stops and then starts
	// again
	if err := container.Mount(); err == nil {
		defer container.Unmount()
	}

	if err := container.Stop(seconds); err != nil {
		return err
	}
	return container.Start()
}

func (container *Container) Resize(h, w int) error {
	if !container.IsRunning() {
		return fmt.Errorf("Cannot resize container %s, container is not running", container.ID)
	}
	return container.command.ProcessConfig.Terminal.Resize(h, w)
}

func (container *Container) Export() (archive.Archive, error) {
	if err := container.Mount(); err != nil {
		return nil, err
	}

	archive, err := archive.Tar(container.basefs, archive.Uncompressed)
	if err != nil {
		container.Unmount()
		return nil, err
	}
	return ioutils.NewReadCloserWrapper(archive, func() error {
			err := archive.Close()
			container.Unmount()
			return err
		}),
		nil
}

func (container *Container) Mount() error {
	return container.daemon.Mount(container)
}

func (container *Container) changes() ([]archive.Change, error) {
	return container.daemon.Changes(container)
}

func (container *Container) Changes() ([]archive.Change, error) {
	container.Lock()
	defer container.Unlock()
	return container.changes()
}

func (container *Container) GetImage() (*image.Image, error) {
	if container.daemon == nil {
		return nil, fmt.Errorf("Can't get image of unregistered container")
	}
	return container.daemon.graph.Get(container.ImageID)
}

func (container *Container) Unmount() error {
	return container.daemon.Unmount(container)
}

func (container *Container) logPath(name string) (string, error) {
	return container.GetRootResourcePath(fmt.Sprintf("%s-%s.log", container.ID, name))
}

func (container *Container) ReadLog(name string) (io.Reader, error) {
	pth, err := container.logPath(name)
	if err != nil {
		return nil, err
	}
	return os.Open(pth)
}

func (container *Container) hostConfigPath() (string, error) {
	return container.GetRootResourcePath("hostconfig.json")
}

func (container *Container) jsonPath() (string, error) {
	return container.GetRootResourcePath("config.json")
}

// This method must be exported to be used from the lxc template
// This directory is only usable when the container is running
func (container *Container) RootfsPath() string {
	return container.basefs
}

func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("Invalid empty id")
	}
	return nil
}

func (container *Container) Copy(resource string) (io.ReadCloser, error) {
	container.Lock()
	defer container.Unlock()
	var err error
	if err := container.Mount(); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			container.Unmount()
		}
	}()

	if err = container.mountVolumes(); err != nil {
		container.unmountVolumes()
		return nil, err
	}
	defer func() {
		if err != nil {
			container.unmountVolumes()
		}
	}()

	basePath, err := container.GetResourcePath(resource)
	if err != nil {
		return nil, err
	}

	stat, err := os.Stat(basePath)
	if err != nil {
		return nil, err
	}
	var filter []string
	if !stat.IsDir() {
		d, f := filepath.Split(basePath)
		basePath = d
		filter = []string{f}
	} else {
		filter = []string{filepath.Base(basePath)}
		basePath = filepath.Dir(basePath)
	}

	archive, err := archive.TarWithOptions(basePath, &archive.TarOptions{
		Compression:  archive.Uncompressed,
		IncludeFiles: filter,
	})
	if err != nil {
		return nil, err
	}

	return ioutils.NewReadCloserWrapper(archive, func() error {
			err := archive.Close()
			container.unmountVolumes()
			container.Unmount()
			return err
		}),
		nil
}

// Returns true if the container exposes a certain port
func (container *Container) Exposes(p nat.Port) bool {
	_, exists := container.Config.ExposedPorts[p]
	return exists
}

func (container *Container) HostConfig() *runconfig.HostConfig {
	container.Lock()
	res := container.hostConfig
	container.Unlock()
	return res
}

func (container *Container) SetHostConfig(hostConfig *runconfig.HostConfig) {
	container.Lock()
	container.hostConfig = hostConfig
	container.Unlock()
}

func (container *Container) DisableLink(name string) {
	if container.activeLinks != nil {
		if link, exists := container.activeLinks[name]; exists {
			link.Disable()
		} else {
			logrus.Debugf("Could not find active link for %s", name)
		}
	}
}

func (container *Container) startLogging() error {
	cfg := container.hostConfig.LogConfig
	if cfg.Type == "" {
		cfg = container.daemon.defaultLogConfig
	}
	var l logger.Logger
	switch cfg.Type {
	case "json-file":
		pth, err := container.logPath("json")
		if err != nil {
			return err
		}
		container.LogPath = pth

		dl, err := jsonfilelog.New(pth)
		if err != nil {
			return err
		}
		l = dl
	case "syslog":
		dl, err := syslog.New(container.ID[:12])
		if err != nil {
			return err
		}
		l = dl
	case "journald":
		dl, err := journald.New(container.ID[:12])
		if err != nil {
			return err
		}
		l = dl
	case "none":
		return nil
	default:
		return fmt.Errorf("Unknown logging driver: %s", cfg.Type)
	}

	copier, err := logger.NewCopier(container.ID, map[string]io.Reader{"stdout": container.StdoutPipe(), "stderr": container.StderrPipe()}, l)
	if err != nil {
		return err
	}
	container.logCopier = copier
	copier.Run()
	container.logDriver = l

	return nil
}

func (container *Container) waitForStart() error {
	container.monitor = newContainerMonitor(container, container.hostConfig.RestartPolicy)

	// block until we either receive an error from the initial start of the container's
	// process or until the process is running in the container
	select {
	case <-container.monitor.startSignal:
	case err := <-promise.Go(container.monitor.Start):
		return err
	}

	return nil
}

func (container *Container) GetProcessLabel() string {
	// even if we have a process label return "" if we are running
	// in privileged mode
	if container.hostConfig.Privileged {
		return ""
	}
	return container.ProcessLabel
}

func (container *Container) GetMountLabel() string {
	if container.hostConfig.Privileged {
		return ""
	}
	return container.MountLabel
}

func (container *Container) Stats() (*execdriver.ResourceStats, error) {
	return container.daemon.Stats(container)
}

func (c *Container) LogDriverType() string {
	c.Lock()
	defer c.Unlock()
	if c.hostConfig.LogConfig.Type == "" {
		return c.daemon.defaultLogConfig.Type
	}
	return c.hostConfig.LogConfig.Type
}
