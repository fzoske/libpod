package libpod

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containers/libpod/libpod/define"
	"github.com/containers/libpod/libpod/events"
	"github.com/containers/libpod/pkg/ctime"
	"github.com/containers/libpod/pkg/hooks"
	"github.com/containers/libpod/pkg/hooks/exec"
	"github.com/containers/libpod/pkg/rootless"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/archive"
	"github.com/containers/storage/pkg/mount"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	// name of the directory holding the artifacts
	artifactsDir      = "artifacts"
	execDirPermission = 0755
)

// rootFsSize gets the size of the container's root filesystem
// A container FS is split into two parts.  The first is the top layer, a
// mutable layer, and the rest is the RootFS: the set of immutable layers
// that make up the image on which the container is based.
func (c *Container) rootFsSize() (int64, error) {
	if c.config.Rootfs != "" {
		return 0, nil
	}
	if c.runtime.store == nil {
		return 0, nil
	}

	container, err := c.runtime.store.Container(c.ID())
	if err != nil {
		return 0, err
	}

	// Ignore the size of the top layer.   The top layer is a mutable RW layer
	// and is not considered a part of the rootfs
	rwLayer, err := c.runtime.store.Layer(container.LayerID)
	if err != nil {
		return 0, err
	}
	layer, err := c.runtime.store.Layer(rwLayer.Parent)
	if err != nil {
		return 0, err
	}

	size := int64(0)
	for layer.Parent != "" {
		layerSize, err := c.runtime.store.DiffSize(layer.Parent, layer.ID)
		if err != nil {
			return size, errors.Wrapf(err, "getting diffsize of layer %q and its parent %q", layer.ID, layer.Parent)
		}
		size += layerSize
		layer, err = c.runtime.store.Layer(layer.Parent)
		if err != nil {
			return 0, err
		}
	}
	// Get the size of the last layer.  Has to be outside of the loop
	// because the parent of the last layer is "", and lstore.Get("")
	// will return an error.
	layerSize, err := c.runtime.store.DiffSize(layer.Parent, layer.ID)
	return size + layerSize, err
}

// rwSize Gets the size of the mutable top layer of the container.
func (c *Container) rwSize() (int64, error) {
	if c.config.Rootfs != "" {
		var size int64
		err := filepath.Walk(c.config.Rootfs, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			size += info.Size()
			return nil
		})
		return size, err
	}

	container, err := c.runtime.store.Container(c.ID())
	if err != nil {
		return 0, err
	}

	// Get the size of the top layer by calculating the size of the diff
	// between the layer and its parent.  The top layer of a container is
	// the only RW layer, all others are immutable
	layer, err := c.runtime.store.Layer(container.LayerID)
	if err != nil {
		return 0, err
	}
	return c.runtime.store.DiffSize(layer.Parent, layer.ID)
}

// bundlePath returns the path to the container's root filesystem - where the OCI spec will be
// placed, amongst other things
func (c *Container) bundlePath() string {
	return c.config.StaticDir
}

// ControlSocketPath returns the path to the containers control socket for things like tty
// resizing
func (c *Container) ControlSocketPath() string {
	return filepath.Join(c.bundlePath(), "ctl")
}

// CheckpointPath returns the path to the directory containing the checkpoint
func (c *Container) CheckpointPath() string {
	return filepath.Join(c.bundlePath(), "checkpoint")
}

// AttachSocketPath retrieves the path of the container's attach socket
func (c *Container) AttachSocketPath() string {
	return filepath.Join(c.ociRuntime.socketsDir, c.ID(), "attach")
}

// exitFilePath gets the path to the container's exit file
func (c *Container) exitFilePath() string {
	return filepath.Join(c.ociRuntime.exitsDir, c.ID())
}

// create a bundle path and associated files for an exec session
func (c *Container) createExecBundle(sessionID string) (err error) {
	bundlePath := c.execBundlePath(sessionID)
	if createErr := os.MkdirAll(bundlePath, execDirPermission); createErr != nil {
		return createErr
	}
	defer func() {
		if err != nil {
			if err2 := os.RemoveAll(bundlePath); err != nil {
				logrus.Warnf("error removing exec bundle after creation caused another error: %v", err2)
			}
		}
	}()
	if err2 := os.MkdirAll(c.execExitFileDir(sessionID), execDirPermission); err2 != nil {
		// The directory is allowed to exist
		if !os.IsExist(err2) {
			err = errors.Wrapf(err2, "error creating OCI runtime exit file path %s", c.execExitFileDir(sessionID))
		}
	}
	return
}

// cleanup an exec session after its done
func (c *Container) cleanupExecBundle(sessionID string) error {
	return os.RemoveAll(c.execBundlePath(sessionID))
}

// the path to a containers exec session bundle
func (c *Container) execBundlePath(sessionID string) string {
	return filepath.Join(c.bundlePath(), sessionID)
}

// Get PID file path for a container's exec session
func (c *Container) execPidPath(sessionID string) string {
	return filepath.Join(c.execBundlePath(sessionID), "exec_pid")
}

// the log path for an exec session
func (c *Container) execLogPath(sessionID string) string {
	return filepath.Join(c.execBundlePath(sessionID), "exec_log")
}

// the socket conmon creates for an exec session
func (c *Container) execAttachSocketPath(sessionID string) string {
	return filepath.Join(c.ociRuntime.socketsDir, sessionID, "attach")
}

// execExitFileDir gets the path to the container's exit file
func (c *Container) execExitFileDir(sessionID string) string {
	return filepath.Join(c.execBundlePath(sessionID), "exit")
}

// execOCILog returns the file path for the exec sessions oci log
func (c *Container) execOCILog(sessionID string) string {
	if !c.ociRuntime.supportsJSON {
		return ""
	}
	return filepath.Join(c.execBundlePath(sessionID), "oci-log")
}

// readExecExitCode reads the exit file for an exec session and returns
// the exit code
func (c *Container) readExecExitCode(sessionID string) (int, error) {
	exitFile := filepath.Join(c.execExitFileDir(sessionID), c.ID())
	chWait := make(chan error)
	defer close(chWait)

	_, err := WaitForFile(exitFile, chWait, time.Second*5)
	if err != nil {
		return -1, err
	}
	ec, err := ioutil.ReadFile(exitFile)
	if err != nil {
		return -1, err
	}
	ecInt, err := strconv.Atoi(string(ec))
	if err != nil {
		return -1, err
	}
	return ecInt, nil
}

// Wait for the container's exit file to appear.
// When it does, update our state based on it.
func (c *Container) waitForExitFileAndSync() error {
	exitFile := c.exitFilePath()

	chWait := make(chan error)
	defer close(chWait)

	_, err := WaitForFile(exitFile, chWait, time.Second*5)
	if err != nil {
		// Exit file did not appear
		// Reset our state
		c.state.ExitCode = -1
		c.state.FinishedTime = time.Now()
		c.state.State = define.ContainerStateStopped

		if err2 := c.save(); err2 != nil {
			logrus.Errorf("Error saving container %s state: %v", c.ID(), err2)
		}

		return err
	}

	if err := c.ociRuntime.updateContainerStatus(c, false); err != nil {
		return err
	}

	return c.save()
}

// Handle the container exit file.
// The exit file is used to supply container exit time and exit code.
// This assumes the exit file already exists.
func (c *Container) handleExitFile(exitFile string, fi os.FileInfo) error {
	c.state.FinishedTime = ctime.Created(fi)
	statusCodeStr, err := ioutil.ReadFile(exitFile)
	if err != nil {
		return errors.Wrapf(err, "failed to read exit file for container %s", c.ID())
	}
	statusCode, err := strconv.Atoi(string(statusCodeStr))
	if err != nil {
		return errors.Wrapf(err, "error converting exit status code (%q) for container %s to int",
			c.ID(), statusCodeStr)
	}
	c.state.ExitCode = int32(statusCode)

	oomFilePath := filepath.Join(c.bundlePath(), "oom")
	if _, err = os.Stat(oomFilePath); err == nil {
		c.state.OOMKilled = true
	}

	c.state.Exited = true

	// Write an event for the container's death
	c.newContainerExitedEvent(c.state.ExitCode)

	return nil
}

// Handle container restart policy.
// This is called when a container has exited, and was not explicitly stopped by
// an API call to stop the container or pod it is in.
func (c *Container) handleRestartPolicy(ctx context.Context) (restarted bool, err error) {
	// If we did not get a restart policy match, exit immediately.
	// Do the same if we're not a policy that restarts.
	if !c.state.RestartPolicyMatch ||
		c.config.RestartPolicy == RestartPolicyNo ||
		c.config.RestartPolicy == RestartPolicyNone {
		return false, nil
	}

	// If we're RestartPolicyOnFailure, we need to check retries and exit
	// code.
	if c.config.RestartPolicy == RestartPolicyOnFailure {
		if c.state.ExitCode == 0 {
			return false, nil
		}

		// If we don't have a max retries set, continue
		if c.config.RestartRetries > 0 {
			if c.state.RestartCount < c.config.RestartRetries {
				logrus.Debugf("Container %s restart policy trigger: on retry %d (of %d)",
					c.ID(), c.state.RestartCount, c.config.RestartRetries)
			} else {
				logrus.Debugf("Container %s restart policy trigger: retries exhausted", c.ID())
				return false, nil
			}
		}
	}

	logrus.Debugf("Restarting container %s due to restart policy %s", c.ID(), c.config.RestartPolicy)

	// Need to check if dependencies are alive.
	if err = c.checkDependenciesAndHandleError(ctx); err != nil {
		return false, err
	}

	// Is the container running again?
	// If so, we don't have to do anything
	if c.state.State == define.ContainerStateRunning || c.state.State == define.ContainerStatePaused {
		return false, nil
	} else if c.state.State == define.ContainerStateUnknown {
		return false, errors.Wrapf(define.ErrInternal, "invalid container state encountered in restart attempt!")
	}

	c.newContainerEvent(events.Restart)

	// Increment restart count
	c.state.RestartCount = c.state.RestartCount + 1
	logrus.Debugf("Container %s now on retry %d", c.ID(), c.state.RestartCount)
	if err := c.save(); err != nil {
		return false, err
	}

	defer func() {
		if err != nil {
			if err2 := c.cleanup(ctx); err2 != nil {
				logrus.Errorf("error cleaning up container %s: %v", c.ID(), err2)
			}
		}
	}()
	if err := c.prepare(); err != nil {
		return false, err
	}

	if c.state.State == define.ContainerStateStopped {
		// Reinitialize the container if we need to
		if err := c.reinit(ctx, true); err != nil {
			return false, err
		}
	} else if c.state.State == define.ContainerStateConfigured ||
		c.state.State == define.ContainerStateExited {
		// Initialize the container
		if err := c.init(ctx, true); err != nil {
			return false, err
		}
	}
	if err := c.start(); err != nil {
		return false, err
	}
	return true, nil
}

// Sync this container with on-disk state and runtime status
// Should only be called with container lock held
// This function should suffice to ensure a container's state is accurate and
// it is valid for use.
func (c *Container) syncContainer() error {
	if err := c.runtime.state.UpdateContainer(c); err != nil {
		return err
	}
	// If runtime knows about the container, update its status in runtime
	// And then save back to disk
	if (c.state.State != define.ContainerStateUnknown) &&
		(c.state.State != define.ContainerStateConfigured) &&
		(c.state.State != define.ContainerStateExited) {
		oldState := c.state.State
		// TODO: optionally replace this with a stat for the exit file
		if err := c.ociRuntime.updateContainerStatus(c, false); err != nil {
			return err
		}
		// Only save back to DB if state changed
		if c.state.State != oldState {
			// Check for a restart policy match
			if c.config.RestartPolicy != RestartPolicyNone && c.config.RestartPolicy != RestartPolicyNo &&
				(oldState == define.ContainerStateRunning || oldState == define.ContainerStatePaused) &&
				(c.state.State == define.ContainerStateStopped || c.state.State == define.ContainerStateExited) &&
				!c.state.StoppedByUser {
				c.state.RestartPolicyMatch = true
			}

			if err := c.save(); err != nil {
				return err
			}
		}
	}

	if !c.valid {
		return errors.Wrapf(define.ErrCtrRemoved, "container %s is not valid", c.ID())
	}

	return nil
}

// Create container root filesystem for use
func (c *Container) setupStorage(ctx context.Context) error {
	span, _ := opentracing.StartSpanFromContext(ctx, "setupStorage")
	span.SetTag("type", "container")
	defer span.Finish()

	if !c.valid {
		return errors.Wrapf(define.ErrCtrRemoved, "container %s is not valid", c.ID())
	}

	if c.state.State != define.ContainerStateConfigured {
		return errors.Wrapf(define.ErrCtrStateInvalid, "container %s must be in Configured state to have storage set up", c.ID())
	}

	// Need both an image ID and image name, plus a bool telling us whether to use the image configuration
	if c.config.Rootfs == "" && (c.config.RootfsImageID == "" || c.config.RootfsImageName == "") {
		return errors.Wrapf(define.ErrInvalidArg, "must provide image ID and image name to use an image")
	}

	options := storage.ContainerOptions{
		IDMappingOptions: storage.IDMappingOptions{
			HostUIDMapping: true,
			HostGIDMapping: true,
		},
		LabelOpts: c.config.LabelOpts,
	}
	if c.restoreFromCheckpoint {
		// If restoring from a checkpoint, the root file-system
		// needs to be mounted with the same SELinux labels as
		// it was mounted previously.
		if options.Flags == nil {
			options.Flags = make(map[string]interface{})
		}
		options.Flags["ProcessLabel"] = c.config.ProcessLabel
		options.Flags["MountLabel"] = c.config.MountLabel
	}
	if c.config.Privileged {
		privOpt := func(opt string) bool {
			for _, privopt := range []string{"nodev", "nosuid", "noexec"} {
				if opt == privopt {
					return true
				}
			}
			return false
		}

		defOptions, err := storage.GetMountOptions(c.runtime.store.GraphDriverName(), c.runtime.store.GraphOptions())
		if err != nil {
			return errors.Wrapf(err, "error getting default mount options")
		}
		var newOptions []string
		for _, opt := range defOptions {
			if !privOpt(opt) {
				newOptions = append(newOptions, opt)
			}
		}
		options.MountOpts = newOptions
	}

	if c.config.Rootfs == "" {
		options.IDMappingOptions = c.config.IDMappings
	}
	containerInfo, err := c.runtime.storageService.CreateContainerStorage(ctx, c.runtime.imageContext, c.config.RootfsImageName, c.config.RootfsImageID, c.config.Name, c.config.ID, options)
	if err != nil {
		return errors.Wrapf(err, "error creating container storage")
	}

	if len(c.config.IDMappings.UIDMap) != 0 || len(c.config.IDMappings.GIDMap) != 0 {
		if err := os.Chown(containerInfo.RunDir, c.RootUID(), c.RootGID()); err != nil {
			return err
		}

		if err := os.Chown(containerInfo.Dir, c.RootUID(), c.RootGID()); err != nil {
			return err
		}
	}

	c.config.ProcessLabel = containerInfo.ProcessLabel
	c.config.MountLabel = containerInfo.MountLabel
	c.config.StaticDir = containerInfo.Dir
	c.state.RunDir = containerInfo.RunDir

	// Set the default Entrypoint and Command
	if containerInfo.Config != nil {
		if c.config.Entrypoint == nil {
			c.config.Entrypoint = containerInfo.Config.Config.Entrypoint
		}
		if c.config.Command == nil {
			c.config.Command = containerInfo.Config.Config.Cmd
		}
	}

	artifacts := filepath.Join(c.config.StaticDir, artifactsDir)
	if err := os.MkdirAll(artifacts, 0755); err != nil {
		return errors.Wrapf(err, "error creating artifacts directory %q", artifacts)
	}

	return nil
}

// Tear down a container's storage prior to removal
func (c *Container) teardownStorage() error {
	if c.state.State == define.ContainerStateRunning || c.state.State == define.ContainerStatePaused {
		return errors.Wrapf(define.ErrCtrStateInvalid, "cannot remove storage for container %s as it is running or paused", c.ID())
	}

	artifacts := filepath.Join(c.config.StaticDir, artifactsDir)
	if err := os.RemoveAll(artifacts); err != nil {
		return errors.Wrapf(err, "error removing container %s artifacts %q", c.ID(), artifacts)
	}

	if err := c.cleanupStorage(); err != nil {
		return errors.Wrapf(err, "failed to cleanup container %s storage", c.ID())
	}

	if err := c.runtime.storageService.DeleteContainer(c.ID()); err != nil {
		// If the container has already been removed, warn but do not
		// error - we wanted it gone, it is already gone.
		// Potentially another tool using containers/storage already
		// removed it?
		if err == storage.ErrNotAContainer || err == storage.ErrContainerUnknown {
			logrus.Warnf("Storage for container %s already removed", c.ID())
			return nil
		}

		return errors.Wrapf(err, "error removing container %s root filesystem", c.ID())
	}

	return nil
}

// Reset resets state fields to default values
// It is performed before a refresh and clears the state after a reboot
// It does not save the results - assumes the database will do that for us
func resetState(state *ContainerState) error {
	state.PID = 0
	state.ConmonPID = 0
	state.Mountpoint = ""
	state.Mounted = false
	if state.State != define.ContainerStateExited {
		state.State = define.ContainerStateConfigured
	}
	state.ExecSessions = make(map[string]*ExecSession)
	state.NetworkStatus = nil
	state.BindMounts = make(map[string]string)
	state.StoppedByUser = false
	state.RestartPolicyMatch = false
	state.RestartCount = 0

	return nil
}

// Refresh refreshes the container's state after a restart.
// Refresh cannot perform any operations that would lock another container.
// We cannot guarantee any other container has a valid lock at the time it is
// running.
func (c *Container) refresh() error {
	// Don't need a full sync, but we do need to update from the database to
	// pick up potentially-missing container state
	if err := c.runtime.state.UpdateContainer(c); err != nil {
		return err
	}

	if !c.valid {
		return errors.Wrapf(define.ErrCtrRemoved, "container %s is not valid - may have been removed", c.ID())
	}

	// We need to get the container's temporary directory from c/storage
	// It was lost in the reboot and must be recreated
	dir, err := c.runtime.storageService.GetRunDir(c.ID())
	if err != nil {
		return errors.Wrapf(err, "error retrieving temporary directory for container %s", c.ID())
	}
	c.state.RunDir = dir

	if len(c.config.IDMappings.UIDMap) != 0 || len(c.config.IDMappings.GIDMap) != 0 {
		info, err := os.Stat(c.runtime.config.TmpDir)
		if err != nil {
			return errors.Wrapf(err, "cannot stat `%s`", c.runtime.config.TmpDir)
		}
		if err := os.Chmod(c.runtime.config.TmpDir, info.Mode()|0111); err != nil {
			return errors.Wrapf(err, "cannot chmod `%s`", c.runtime.config.TmpDir)
		}
		root := filepath.Join(c.runtime.config.TmpDir, "containers-root", c.ID())
		if err := os.MkdirAll(root, 0755); err != nil {
			return errors.Wrapf(err, "error creating userNS tmpdir for container %s", c.ID())
		}
		if err := os.Chown(root, c.RootUID(), c.RootGID()); err != nil {
			return err
		}
	}

	// We need to pick up a new lock
	lock, err := c.runtime.lockManager.AllocateAndRetrieveLock(c.config.LockID)
	if err != nil {
		return errors.Wrapf(err, "error acquiring lock %d for container %s", c.config.LockID, c.ID())
	}
	c.lock = lock

	if err := c.save(); err != nil {
		return errors.Wrapf(err, "error refreshing state for container %s", c.ID())
	}

	// Remove ctl and attach files, which may persist across reboot
	if err := c.removeConmonFiles(); err != nil {
		return err
	}

	return nil
}

// Remove conmon attach socket and terminal resize FIFO
// This is necessary for restarting containers
func (c *Container) removeConmonFiles() error {
	// Files are allowed to not exist, so ignore ENOENT
	attachFile := filepath.Join(c.bundlePath(), "attach")
	if err := os.Remove(attachFile); err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "error removing container %s attach file", c.ID())
	}

	ctlFile := filepath.Join(c.bundlePath(), "ctl")
	if err := os.Remove(ctlFile); err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "error removing container %s ctl file", c.ID())
	}

	oomFile := filepath.Join(c.bundlePath(), "oom")
	if err := os.Remove(oomFile); err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "error removing container %s OOM file", c.ID())
	}

	// Instead of outright deleting the exit file, rename it (if it exists).
	// We want to retain it so we can get the exit code of containers which
	// are removed (at least until we have a workable events system)
	exitFile := filepath.Join(c.ociRuntime.exitsDir, c.ID())
	oldExitFile := filepath.Join(c.ociRuntime.exitsDir, fmt.Sprintf("%s-old", c.ID()))
	if _, err := os.Stat(exitFile); err != nil {
		if !os.IsNotExist(err) {
			return errors.Wrapf(err, "error running stat on container %s exit file", c.ID())
		}
	} else {
		// Rename should replace the old exit file (if it exists)
		if err := os.Rename(exitFile, oldExitFile); err != nil {
			return errors.Wrapf(err, "error renaming container %s exit file", c.ID())
		}
	}

	return nil
}

func (c *Container) export(path string) error {
	mountPoint := c.state.Mountpoint
	if !c.state.Mounted {
		containerMount, err := c.runtime.store.Mount(c.ID(), c.config.MountLabel)
		if err != nil {
			return errors.Wrapf(err, "error mounting container %q", c.ID())
		}
		mountPoint = containerMount
		defer func() {
			if _, err := c.runtime.store.Unmount(c.ID(), false); err != nil {
				logrus.Errorf("error unmounting container %q: %v", c.ID(), err)
			}
		}()
	}

	input, err := archive.Tar(mountPoint, archive.Uncompressed)
	if err != nil {
		return errors.Wrapf(err, "error reading container directory %q", c.ID())
	}

	outFile, err := os.Create(path)
	if err != nil {
		return errors.Wrapf(err, "error creating file %q", path)
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, input)
	return err
}

// Get path of artifact with a given name for this container
func (c *Container) getArtifactPath(name string) string {
	return filepath.Join(c.config.StaticDir, artifactsDir, name)
}

// Used with Wait() to determine if a container has exited
func (c *Container) isStopped() (bool, error) {
	if !c.batched {
		c.lock.Lock()
		defer c.lock.Unlock()
	}
	err := c.syncContainer()
	if err != nil {
		return true, err
	}
	return c.state.State != define.ContainerStateRunning && c.state.State != define.ContainerStatePaused, nil
}

// save container state to the database
func (c *Container) save() error {
	if err := c.runtime.state.SaveContainer(c); err != nil {
		return errors.Wrapf(err, "error saving container %s state", c.ID())
	}
	return nil
}

// Checks the container is in the right state, then initializes the container in preparation to start the container.
// If recursive is true, each of the containers dependencies will be started.
// Otherwise, this function will return with error if there are dependencies of this container that aren't running.
func (c *Container) prepareToStart(ctx context.Context, recursive bool) (err error) {
	// Container must be created or stopped to be started
	if !(c.state.State == define.ContainerStateConfigured ||
		c.state.State == define.ContainerStateCreated ||
		c.state.State == define.ContainerStateStopped ||
		c.state.State == define.ContainerStateExited) {
		return errors.Wrapf(define.ErrCtrStateInvalid, "container %s must be in Created or Stopped state to be started", c.ID())
	}

	if !recursive {
		if err := c.checkDependenciesAndHandleError(ctx); err != nil {
			return err
		}
	} else {
		if err := c.startDependencies(ctx); err != nil {
			return err
		}
	}

	defer func() {
		if err != nil {
			if err2 := c.cleanup(ctx); err2 != nil {
				logrus.Errorf("error cleaning up container %s: %v", c.ID(), err2)
			}
		}
	}()

	if err := c.prepare(); err != nil {
		return err
	}

	if c.state.State == define.ContainerStateStopped {
		// Reinitialize the container if we need to
		if err := c.reinit(ctx, false); err != nil {
			return err
		}
	} else if c.state.State == define.ContainerStateConfigured ||
		c.state.State == define.ContainerStateExited {
		// Or initialize it if necessary
		if err := c.init(ctx, false); err != nil {
			return err
		}
	}
	return nil
}

// checks dependencies are running and prints a helpful message
func (c *Container) checkDependenciesAndHandleError(ctx context.Context) error {
	notRunning, err := c.checkDependenciesRunning()
	if err != nil {
		return errors.Wrapf(err, "error checking dependencies for container %s", c.ID())
	}
	if len(notRunning) > 0 {
		depString := strings.Join(notRunning, ",")
		return errors.Wrapf(define.ErrCtrStateInvalid, "some dependencies of container %s are not started: %s", c.ID(), depString)
	}

	return nil
}

// Recursively start all dependencies of a container so the container can be started.
func (c *Container) startDependencies(ctx context.Context) error {
	depCtrIDs := c.Dependencies()
	if len(depCtrIDs) == 0 {
		return nil
	}

	depVisitedCtrs := make(map[string]*Container)
	if err := c.getAllDependencies(depVisitedCtrs); err != nil {
		return errors.Wrapf(err, "error starting dependency for container %s", c.ID())
	}

	// Because of how Go handles passing slices through functions, a slice cannot grow between function calls
	// without clunky syntax. Circumnavigate this by translating the map to a slice for buildContainerGraph
	depCtrs := make([]*Container, 0)
	for _, ctr := range depVisitedCtrs {
		depCtrs = append(depCtrs, ctr)
	}

	// Build a dependency graph of containers
	graph, err := buildContainerGraph(depCtrs)
	if err != nil {
		return errors.Wrapf(err, "error generating dependency graph for container %s", c.ID())
	}

	// If there are no containers without dependencies, we can't start
	// Error out
	if len(graph.noDepNodes) == 0 {
		// we have no dependencies that need starting, go ahead and return
		if len(graph.nodes) == 0 {
			return nil
		}
		return errors.Wrapf(define.ErrNoSuchCtr, "All dependencies have dependencies of %s", c.ID())
	}

	ctrErrors := make(map[string]error)
	ctrsVisited := make(map[string]bool)

	// Traverse the graph beginning at nodes with no dependencies
	for _, node := range graph.noDepNodes {
		startNode(ctx, node, false, ctrErrors, ctrsVisited, true)
	}

	if len(ctrErrors) > 0 {
		logrus.Errorf("error starting some container dependencies")
		for _, e := range ctrErrors {
			logrus.Errorf("%q", e)
		}
		return errors.Wrapf(define.ErrInternal, "error starting some containers")
	}
	return nil
}

// getAllDependencies is a precursor to starting dependencies.
// To start a container with all of its dependencies, we need to recursively find all dependencies
// a container has, as well as each of those containers' dependencies, and so on
// To do so, keep track of containers already visisted (so there aren't redundant state lookups),
// and recursively search until we have reached the leafs of every dependency node.
// Since we need to start all dependencies for our original container to successfully start, we propegate any errors
// in looking up dependencies.
// Note: this function is currently meant as a robust solution to a narrow problem: start an infra-container when
// a container in the pod is run. It has not been tested for performance past one level, so expansion of recursive start
// must be tested first.
func (c *Container) getAllDependencies(visited map[string]*Container) error {
	depIDs := c.Dependencies()
	if len(depIDs) == 0 {
		return nil
	}
	for _, depID := range depIDs {
		if _, ok := visited[depID]; !ok {
			dep, err := c.runtime.state.Container(depID)
			if err != nil {
				return err
			}
			status, err := dep.State()
			if err != nil {
				return err
			}
			// if the dependency is already running, we can assume its dependencies are also running
			// so no need to add them to those we need to start
			if status != define.ContainerStateRunning {
				visited[depID] = dep
				if err := dep.getAllDependencies(visited); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Check if a container's dependencies are running
// Returns a []string containing the IDs of dependencies that are not running
func (c *Container) checkDependenciesRunning() ([]string, error) {
	deps := c.Dependencies()
	notRunning := []string{}

	// We were not passed a set of dependency containers
	// Make it ourselves
	depCtrs := make(map[string]*Container, len(deps))
	for _, dep := range deps {
		// Get the dependency container
		depCtr, err := c.runtime.state.Container(dep)
		if err != nil {
			return nil, errors.Wrapf(err, "error retrieving dependency %s of container %s from state", dep, c.ID())
		}

		// Check the status
		state, err := depCtr.State()
		if err != nil {
			return nil, errors.Wrapf(err, "error retrieving state of dependency %s of container %s", dep, c.ID())
		}
		if state != define.ContainerStateRunning {
			notRunning = append(notRunning, dep)
		}
		depCtrs[dep] = depCtr
	}

	return notRunning, nil
}

func (c *Container) completeNetworkSetup() error {
	netDisabled, err := c.NetworkDisabled()
	if err != nil {
		return err
	}
	if !c.config.PostConfigureNetNS || netDisabled {
		return nil
	}
	if err := c.syncContainer(); err != nil {
		return err
	}
	if c.config.NetMode == "slirp4netns" {
		return c.runtime.setupRootlessNetNS(c)
	}
	return c.runtime.setupNetNS(c)
}

// Initialize a container, creating it in the runtime
func (c *Container) init(ctx context.Context, retainRetries bool) error {
	span, _ := opentracing.StartSpanFromContext(ctx, "init")
	span.SetTag("struct", "container")
	defer span.Finish()

	// Generate the OCI newSpec
	newSpec, err := c.generateSpec(ctx)
	if err != nil {
		return err
	}

	// Save the OCI newSpec to disk
	if err := c.saveSpec(newSpec); err != nil {
		return err
	}

	// With the spec complete, do an OCI create
	if err := c.ociRuntime.createContainer(c, nil); err != nil {
		return err
	}

	logrus.Debugf("Created container %s in OCI runtime", c.ID())

	c.state.ExitCode = 0
	c.state.Exited = false
	c.state.State = define.ContainerStateCreated
	c.state.StoppedByUser = false
	c.state.RestartPolicyMatch = false

	if !retainRetries {
		c.state.RestartCount = 0
	}

	if err := c.save(); err != nil {
		return err
	}
	if c.config.HealthCheckConfig != nil {
		if err := c.createTimer(); err != nil {
			logrus.Error(err)
		}
	}

	defer c.newContainerEvent(events.Init)
	return c.completeNetworkSetup()
}

// Clean up a container in the OCI runtime.
// Deletes the container in the runtime, and resets its state to Exited.
// The container can be restarted cleanly after this.
func (c *Container) cleanupRuntime(ctx context.Context) error {
	span, _ := opentracing.StartSpanFromContext(ctx, "cleanupRuntime")
	span.SetTag("struct", "container")
	defer span.Finish()

	// If the container is not ContainerStateStopped or
	// ContainerStateCreated, do nothing.
	if c.state.State != define.ContainerStateStopped && c.state.State != define.ContainerStateCreated {
		return nil
	}

	// If necessary, delete attach and ctl files
	if err := c.removeConmonFiles(); err != nil {
		return err
	}

	if err := c.delete(ctx); err != nil {
		return err
	}

	// If we were Stopped, we are now Exited, as we've removed ourself
	// from the runtime.
	// If we were Created, we are now Configured.
	if c.state.State == define.ContainerStateStopped {
		c.state.State = define.ContainerStateExited
	} else if c.state.State == define.ContainerStateCreated {
		c.state.State = define.ContainerStateConfigured
	}

	if c.valid {
		if err := c.save(); err != nil {
			return err
		}
	}

	logrus.Debugf("Successfully cleaned up container %s", c.ID())

	return nil
}

// Reinitialize a container.
// Deletes and recreates a container in the runtime.
// Should only be done on ContainerStateStopped containers.
// Not necessary for ContainerStateExited - the container has already been
// removed from the runtime, so init() can proceed freely.
func (c *Container) reinit(ctx context.Context, retainRetries bool) error {
	span, _ := opentracing.StartSpanFromContext(ctx, "reinit")
	span.SetTag("struct", "container")
	defer span.Finish()

	logrus.Debugf("Recreating container %s in OCI runtime", c.ID())

	if err := c.cleanupRuntime(ctx); err != nil {
		return err
	}

	// Initialize the container again
	return c.init(ctx, retainRetries)
}

// Initialize (if necessary) and start a container
// Performs all necessary steps to start a container that is not running
// Does not lock or check validity
func (c *Container) initAndStart(ctx context.Context) (err error) {
	// If we are ContainerStateUnknown, throw an error
	if c.state.State == define.ContainerStateUnknown {
		return errors.Wrapf(define.ErrCtrStateInvalid, "container %s is in an unknown state", c.ID())
	}

	// If we are running, do nothing
	if c.state.State == define.ContainerStateRunning {
		return nil
	}
	// If we are paused, throw an error
	if c.state.State == define.ContainerStatePaused {
		return errors.Wrapf(define.ErrCtrStateInvalid, "cannot start paused container %s", c.ID())
	}

	defer func() {
		if err != nil {
			if err2 := c.cleanup(ctx); err2 != nil {
				logrus.Errorf("error cleaning up container %s: %v", c.ID(), err2)
			}
		}
	}()

	if err := c.prepare(); err != nil {
		return err
	}

	// If we are ContainerStateStopped we need to remove from runtime
	// And reset to ContainerStateConfigured
	if c.state.State == define.ContainerStateStopped {
		logrus.Debugf("Recreating container %s in OCI runtime", c.ID())

		if err := c.reinit(ctx, false); err != nil {
			return err
		}
	} else if c.state.State == define.ContainerStateConfigured ||
		c.state.State == define.ContainerStateExited {
		if err := c.init(ctx, false); err != nil {
			return err
		}
	}

	// Now start the container
	return c.start()
}

// Internal, non-locking function to start a container
func (c *Container) start() error {
	if c.config.Spec.Process != nil {
		logrus.Debugf("Starting container %s with command %v", c.ID(), c.config.Spec.Process.Args)
	}

	if err := c.ociRuntime.startContainer(c); err != nil {
		return err
	}
	logrus.Debugf("Started container %s", c.ID())

	c.state.State = define.ContainerStateRunning

	if c.config.HealthCheckConfig != nil {
		if err := c.updateHealthStatus(HealthCheckStarting); err != nil {
			logrus.Error(err)
		}
		if err := c.startTimer(); err != nil {
			logrus.Error(err)
		}
	}

	defer c.newContainerEvent(events.Start)

	return c.save()
}

// Internal, non-locking function to stop container
func (c *Container) stop(timeout uint) error {
	logrus.Debugf("Stopping ctr %s (timeout %d)", c.ID(), timeout)

	if err := c.ociRuntime.stopContainer(c, timeout); err != nil {
		return err
	}

	c.state.PID = 0
	c.state.ConmonPID = 0
	c.state.StoppedByUser = true
	if err := c.save(); err != nil {
		return errors.Wrapf(err, "error saving container %s state after stopping", c.ID())
	}

	// Wait until we have an exit file, and sync once we do
	return c.waitForExitFileAndSync()
}

// Internal, non-locking function to pause a container
func (c *Container) pause() error {
	if err := c.ociRuntime.pauseContainer(c); err != nil {
		return err
	}

	logrus.Debugf("Paused container %s", c.ID())

	c.state.State = define.ContainerStatePaused

	return c.save()
}

// Internal, non-locking function to unpause a container
func (c *Container) unpause() error {
	if err := c.ociRuntime.unpauseContainer(c); err != nil {
		return err
	}

	logrus.Debugf("Unpaused container %s", c.ID())

	c.state.State = define.ContainerStateRunning

	return c.save()
}

// Internal, non-locking function to restart a container
func (c *Container) restartWithTimeout(ctx context.Context, timeout uint) (err error) {
	if c.state.State == define.ContainerStateUnknown || c.state.State == define.ContainerStatePaused {
		return errors.Wrapf(define.ErrCtrStateInvalid, "unable to restart a container in a paused or unknown state")
	}

	c.newContainerEvent(events.Restart)

	if c.state.State == define.ContainerStateRunning {
		if err := c.stop(timeout); err != nil {
			return err
		}
	}
	defer func() {
		if err != nil {
			if err2 := c.cleanup(ctx); err2 != nil {
				logrus.Errorf("error cleaning up container %s: %v", c.ID(), err2)
			}
		}
	}()
	if err := c.prepare(); err != nil {
		return err
	}

	if c.state.State == define.ContainerStateStopped {
		// Reinitialize the container if we need to
		if err := c.reinit(ctx, false); err != nil {
			return err
		}
	} else if c.state.State == define.ContainerStateConfigured ||
		c.state.State == define.ContainerStateExited {
		// Initialize the container
		if err := c.init(ctx, false); err != nil {
			return err
		}
	}
	return c.start()
}

// mountStorage sets up the container's root filesystem
// It mounts the image and any other requested mounts
// TODO: Add ability to override mount label so we can use this for Mount() too
// TODO: Can we use this for export? Copying SHM into the export might not be
// good
func (c *Container) mountStorage() (string, error) {
	var err error
	// Container already mounted, nothing to do
	if c.state.Mounted {
		return c.state.Mountpoint, nil
	}

	mounted, err := mount.Mounted(c.config.ShmDir)
	if err != nil {
		return "", errors.Wrapf(err, "unable to determine if %q is mounted", c.config.ShmDir)
	}

	if !mounted && !MountExists(c.config.Spec.Mounts, "/dev/shm") {
		shmOptions := fmt.Sprintf("mode=1777,size=%d", c.config.ShmSize)
		if err := c.mountSHM(shmOptions); err != nil {
			return "", err
		}
		if err := os.Chown(c.config.ShmDir, c.RootUID(), c.RootGID()); err != nil {
			return "", errors.Wrapf(err, "failed to chown %s", c.config.ShmDir)
		}
	}

	// TODO: generalize this mount code so it will mount every mount in ctr.config.Mounts
	mountPoint := c.config.Rootfs
	if mountPoint == "" {
		mountPoint, err = c.mount()
		if err != nil {
			return "", err
		}
	}

	return mountPoint, nil
}

// cleanupStorage unmounts and cleans up the container's root filesystem
func (c *Container) cleanupStorage() error {
	if !c.state.Mounted {
		// Already unmounted, do nothing
		logrus.Debugf("Container %s storage is already unmounted, skipping...", c.ID())
		return nil
	}

	for _, containerMount := range c.config.Mounts {
		if err := c.unmountSHM(containerMount); err != nil {
			return err
		}
	}

	if c.config.Rootfs != "" {
		return nil
	}

	if err := c.unmount(false); err != nil {
		// If the container has already been removed, warn but don't
		// error
		// We still want to be able to kick the container out of the
		// state
		if errors.Cause(err) == storage.ErrNotAContainer || errors.Cause(err) == storage.ErrContainerUnknown {
			logrus.Errorf("Storage for container %s has been removed", c.ID())
			return nil
		}

		return err
	}

	c.state.Mountpoint = ""
	c.state.Mounted = false

	if c.valid {
		return c.save()
	}
	return nil
}

// Unmount the a container and free its resources
func (c *Container) cleanup(ctx context.Context) error {
	var lastError error

	span, _ := opentracing.StartSpanFromContext(ctx, "cleanup")
	span.SetTag("struct", "container")
	defer span.Finish()

	logrus.Debugf("Cleaning up container %s", c.ID())

	// Remove healthcheck unit/timer file if it execs
	if c.config.HealthCheckConfig != nil {
		if err := c.removeTimer(); err != nil {
			logrus.Errorf("Error removing timer for container %s healthcheck: %v", c.ID(), err)
		}
	}

	// Clean up network namespace, if present
	if err := c.cleanupNetwork(); err != nil {
		lastError = errors.Wrapf(err, "error removing container %s network", c.ID())
	}

	// Unmount storage
	if err := c.cleanupStorage(); err != nil {
		if lastError != nil {
			logrus.Errorf("Error unmounting container %s storage: %v", c.ID(), err)
		} else {
			lastError = errors.Wrapf(err, "error unmounting container %s storage", c.ID())
		}
	}

	// Remove the container from the runtime, if necessary
	if err := c.cleanupRuntime(ctx); err != nil {
		if lastError != nil {
			logrus.Errorf("Error removing container %s from OCI runtime: %v", c.ID(), err)
		} else {
			lastError = err
		}
	}

	return lastError
}

// delete deletes the container and runs any configured poststop
// hooks.
func (c *Container) delete(ctx context.Context) (err error) {
	span, _ := opentracing.StartSpanFromContext(ctx, "delete")
	span.SetTag("struct", "container")
	defer span.Finish()

	if err := c.ociRuntime.deleteContainer(c); err != nil {
		return errors.Wrapf(err, "error removing container %s from runtime", c.ID())
	}

	if err := c.postDeleteHooks(ctx); err != nil {
		return errors.Wrapf(err, "container %s poststop hooks", c.ID())
	}

	return nil
}

// postDeleteHooks runs the poststop hooks (if any) as specified by
// the OCI Runtime Specification (which requires them to run
// post-delete, despite the stage name).
func (c *Container) postDeleteHooks(ctx context.Context) (err error) {
	span, _ := opentracing.StartSpanFromContext(ctx, "postDeleteHooks")
	span.SetTag("struct", "container")
	defer span.Finish()

	if c.state.ExtensionStageHooks != nil {
		extensionHooks, ok := c.state.ExtensionStageHooks["poststop"]
		if ok {
			state, err := json.Marshal(spec.State{
				Version:     spec.Version,
				ID:          c.ID(),
				Status:      "stopped",
				Bundle:      c.bundlePath(),
				Annotations: c.config.Spec.Annotations,
			})
			if err != nil {
				return err
			}
			for i, hook := range extensionHooks {
				hook := hook
				logrus.Debugf("container %s: invoke poststop hook %d, path %s", c.ID(), i, hook.Path)
				var stderr, stdout bytes.Buffer
				hookErr, err := exec.Run(ctx, &hook, state, &stdout, &stderr, exec.DefaultPostKillTimeout)
				if err != nil {
					logrus.Warnf("container %s: poststop hook %d: %v", c.ID(), i, err)
					if hookErr != err {
						logrus.Debugf("container %s: poststop hook %d (hook error): %v", c.ID(), i, hookErr)
					}
					stdoutString := stdout.String()
					if stdoutString != "" {
						logrus.Debugf("container %s: poststop hook %d: stdout:\n%s", c.ID(), i, stdoutString)
					}
					stderrString := stderr.String()
					if stderrString != "" {
						logrus.Debugf("container %s: poststop hook %d: stderr:\n%s", c.ID(), i, stderrString)
					}
				}
			}
		}
	}

	return nil
}

// writeStringToRundir copies the provided file to the runtimedir
func (c *Container) writeStringToRundir(destFile, output string) (string, error) {
	destFileName := filepath.Join(c.state.RunDir, destFile)

	if err := os.Remove(destFileName); err != nil && !os.IsNotExist(err) {
		return "", errors.Wrapf(err, "error removing %s for container %s", destFile, c.ID())
	}

	f, err := os.Create(destFileName)
	if err != nil {
		return "", errors.Wrapf(err, "unable to create %s", destFileName)
	}
	defer f.Close()
	if err := f.Chown(c.RootUID(), c.RootGID()); err != nil {
		return "", err
	}

	if _, err := f.WriteString(output); err != nil {
		return "", errors.Wrapf(err, "unable to write %s", destFileName)
	}
	// Relabel runDirResolv for the container
	if err := label.Relabel(destFileName, c.config.MountLabel, false); err != nil {
		return "", err
	}

	return filepath.Join(c.state.RunDir, destFile), nil
}

// appendStringToRundir appends the provided string to the runtimedir file
func (c *Container) appendStringToRundir(destFile, output string) (string, error) {
	destFileName := filepath.Join(c.state.RunDir, destFile)

	f, err := os.OpenFile(destFileName, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return "", errors.Wrapf(err, "unable to open %s", destFileName)
	}
	defer f.Close()

	if _, err := f.WriteString(output); err != nil {
		return "", errors.Wrapf(err, "unable to write %s", destFileName)
	}

	return filepath.Join(c.state.RunDir, destFile), nil
}

// saveSpec saves the OCI spec to disk, replacing any existing specs for the container
func (c *Container) saveSpec(spec *spec.Spec) error {
	// If the OCI spec already exists, we need to replace it
	// Cannot guarantee some things, e.g. network namespaces, have the same
	// paths
	jsonPath := filepath.Join(c.bundlePath(), "config.json")
	if _, err := os.Stat(jsonPath); err != nil {
		if !os.IsNotExist(err) {
			return errors.Wrapf(err, "error doing stat on container %s spec", c.ID())
		}
		// The spec does not exist, we're fine
	} else {
		// The spec exists, need to remove it
		if err := os.Remove(jsonPath); err != nil {
			return errors.Wrapf(err, "error replacing runtime spec for container %s", c.ID())
		}
	}

	fileJSON, err := json.Marshal(spec)
	if err != nil {
		return errors.Wrapf(err, "error exporting runtime spec for container %s to JSON", c.ID())
	}
	if err := ioutil.WriteFile(jsonPath, fileJSON, 0644); err != nil {
		return errors.Wrapf(err, "error writing runtime spec JSON for container %s to disk", c.ID())
	}

	logrus.Debugf("Created OCI spec for container %s at %s", c.ID(), jsonPath)

	c.state.ConfigPath = jsonPath

	return nil
}

// Warning: precreate hooks may alter 'config' in place.
func (c *Container) setupOCIHooks(ctx context.Context, config *spec.Spec) (extensionStageHooks map[string][]spec.Hook, err error) {
	allHooks := make(map[string][]spec.Hook)
	if c.runtime.config.HooksDir == nil {
		if rootless.IsRootless() {
			return nil, nil
		}
		for _, hDir := range []string{hooks.DefaultDir, hooks.OverrideDir} {
			manager, err := hooks.New(ctx, []string{hDir}, []string{"precreate", "poststop"})
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			ociHooks, err := manager.Hooks(config, c.Spec().Annotations, len(c.config.UserVolumes) > 0)
			if err != nil {
				return nil, err
			}
			if len(ociHooks) > 0 || config.Hooks != nil {
				logrus.Warnf("implicit hook directories are deprecated; set --ociHooks-dir=%q explicitly to continue to load ociHooks from this directory", hDir)
			}
			for i, hook := range ociHooks {
				allHooks[i] = hook
			}
		}
	} else {
		manager, err := hooks.New(ctx, c.runtime.config.HooksDir, []string{"precreate", "poststop"})
		if err != nil {
			return nil, err
		}

		allHooks, err = manager.Hooks(config, c.Spec().Annotations, len(c.config.UserVolumes) > 0)
		if err != nil {
			return nil, err
		}
	}

	hookErr, err := exec.RuntimeConfigFilter(ctx, allHooks["precreate"], config, exec.DefaultPostKillTimeout)
	if err != nil {
		logrus.Warnf("container %s: precreate hook: %v", c.ID(), err)
		if hookErr != nil && hookErr != err {
			logrus.Debugf("container %s: precreate hook (hook error): %v", c.ID(), hookErr)
		}
		return nil, err
	}

	return allHooks, nil
}

// mount mounts the container's root filesystem
func (c *Container) mount() (string, error) {
	mountPoint, err := c.runtime.storageService.MountContainerImage(c.ID())
	if err != nil {
		return "", errors.Wrapf(err, "error mounting storage for container %s", c.ID())
	}
	mountPoint, err = filepath.EvalSymlinks(mountPoint)
	if err != nil {
		return "", errors.Wrapf(err, "error resolving storage path for container %s", c.ID())
	}
	if err := os.Chown(mountPoint, c.RootUID(), c.RootGID()); err != nil {
		return "", errors.Wrapf(err, "cannot chown %s to %d:%d", mountPoint, c.RootUID(), c.RootGID())
	}
	return mountPoint, nil
}

// unmount unmounts the container's root filesystem
func (c *Container) unmount(force bool) error {
	// Also unmount storage
	if _, err := c.runtime.storageService.UnmountContainerImage(c.ID(), force); err != nil {
		return errors.Wrapf(err, "error unmounting container %s root filesystem", c.ID())
	}

	return nil
}

// this should be from chrootarchive.
func (c *Container) copyWithTarFromImage(src, dest string) error {
	mountpoint, err := c.mount()
	if err != nil {
		return err
	}
	a := archive.NewDefaultArchiver()
	source := filepath.Join(mountpoint, src)

	if err = c.copyOwnerAndPerms(source, dest); err != nil {
		return err
	}
	return a.CopyWithTar(source, dest)
}

// checkReadyForRemoval checks whether the given container is ready to be
// removed.
// These checks are only used if force-remove is not specified.
// If it is, we'll remove the container anyways.
// Returns nil if safe to remove, or an error describing why it's unsafe if not.
func (c *Container) checkReadyForRemoval() error {
	if c.state.State == define.ContainerStateUnknown {
		return errors.Wrapf(define.ErrCtrStateInvalid, "container %s is in invalid state", c.ID())
	}

	if c.state.State == define.ContainerStateRunning ||
		c.state.State == define.ContainerStatePaused {
		return errors.Wrapf(define.ErrCtrStateInvalid, "cannot remove container %s as it is %s - running or paused containers cannot be removed", c.ID(), c.state.State.String())
	}

	if len(c.state.ExecSessions) != 0 {
		return errors.Wrapf(define.ErrCtrStateInvalid, "cannot remove container %s as it has active exec sessions", c.ID())
	}

	return nil
}

// writeJSONFile marshalls and writes the given data to a JSON file
// in the bundle path
func (c *Container) writeJSONFile(v interface{}, file string) (err error) {
	fileJSON, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errors.Wrapf(err, "error writing JSON to %s for container %s", file, c.ID())
	}
	file = filepath.Join(c.bundlePath(), file)
	if err := ioutil.WriteFile(file, fileJSON, 0644); err != nil {
		return err
	}

	return nil
}

// prepareCheckpointExport writes the config and spec to
// JSON files for later export
func (c *Container) prepareCheckpointExport() (err error) {
	// save live config
	if err := c.writeJSONFile(c.Config(), "config.dump"); err != nil {
		return err
	}

	// save spec
	jsonPath := filepath.Join(c.bundlePath(), "config.json")
	g, err := generate.NewFromFile(jsonPath)
	if err != nil {
		logrus.Debugf("generating spec for container %q failed with %v", c.ID(), err)
		return err
	}
	if err := c.writeJSONFile(g.Config, "spec.dump"); err != nil {
		return err
	}

	return nil
}

// sortUserVolumes sorts the volumes specified for a container
// between named and normal volumes
func (c *Container) sortUserVolumes(ctrSpec *spec.Spec) ([]*ContainerNamedVolume, []spec.Mount) {
	namedUserVolumes := []*ContainerNamedVolume{}
	userMounts := []spec.Mount{}

	// We need to parse all named volumes and mounts into maps, so we don't
	// end up with repeated lookups for each user volume.
	// Map destination to struct, as destination is what is stored in
	// UserVolumes.
	namedVolumes := make(map[string]*ContainerNamedVolume)
	mounts := make(map[string]spec.Mount)
	for _, namedVol := range c.config.NamedVolumes {
		namedVolumes[namedVol.Dest] = namedVol
	}
	for _, mount := range ctrSpec.Mounts {
		mounts[mount.Destination] = mount
	}

	for _, vol := range c.config.UserVolumes {
		if volume, ok := namedVolumes[vol]; ok {
			namedUserVolumes = append(namedUserVolumes, volume)
		} else if mount, ok := mounts[vol]; ok {
			userMounts = append(userMounts, mount)
		} else {
			logrus.Warnf("Could not find mount at destination %q when parsing user volumes for container %s", vol, c.ID())
		}
	}
	return namedUserVolumes, userMounts
}
