package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"github.com/cloudflare/cfssl/log"
	"golang.org/x/sys/windows"
	"os"
	"path"
	"strings"
	"time"

	"github.com/containerd/containerd/leases"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/containerfs"
	"github.com/opencontainers/selinux/go-selinux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// ContainerRm removes the container id from the filesystem. An error
// is returned if the container is not found, or if the remove
// fails. If the remove succeeds, the container name is released, and
// network links are removed.
func (daemon *Daemon) ContainerRm(name string, config *types.ContainerRmConfig) error {
	logrus.Debugf("daemon.ContainerRm: name %s", name)
	start := time.Now()
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}

	if ctr.Pid == 0 {
		logrus.Debugf("onlyCleanupContainer1: id %s pid %d", ctr.ID, ctr.Pid)
		daemon.onlyCleanupContainer(ctr, *config, true)
		containerActions.WithValues("delete").UpdateSince(start)
		return nil
	}
	recordsContainerId(ctr.ID)
	logrus.Debugf("setRemovalInProgress: name: %s", name)
	// Container state RemovalInProgress should be used to avoid races.
	if inProgress := ctr.SetRemovalInProgress(); inProgress {
		err := fmt.Errorf("removal of container %s is already in progress", name)
		return errdefs.Conflict(err)
	}
	defer ctr.ResetRemovalInProgress()

	logrus.Debugf("daemon.ContainerRm Get: id %s", ctr.ID)
	// check if container wasn't deregistered by previous rm since Get
	if c := daemon.containers.Get(ctr.ID); c == nil {
		return nil
	}

	if config.RemoveLink {
		return daemon.rmLink(ctr, name)
	}

	logrus.Debugf("daemon.ContainerRm cleanupContainer: id %s", ctr.ID)
	err = daemon.cleanupContainer(ctr, *config)
	containerActions.WithValues("delete").UpdateSince(start)

	return err
}

func (daemon *Daemon) rmLink(container *container.Container, name string) error {
	if name[0] != '/' {
		name = "/" + name
	}
	parent, n := path.Split(name)
	if parent == "/" {
		return fmt.Errorf("Conflict, cannot remove the default link name of the container")
	}

	parent = strings.TrimSuffix(parent, "/")
	pe, err := daemon.containersReplica.Snapshot().GetID(parent)
	if err != nil {
		return fmt.Errorf("Cannot get parent %s for link name %s", parent, name)
	}

	daemon.releaseName(name)
	parentContainer, _ := daemon.GetContainer(pe)
	if parentContainer != nil {
		daemon.linkIndex.unlink(name, container, parentContainer)
		if err := daemon.updateNetwork(parentContainer); err != nil {
			logrus.Debugf("Could not update network to remove link %s: %v", n, err)
		}
	}
	return nil
}

// cleanupContainer unregisters a container from the daemon, stops stats
// collection and cleanly removes contents and metadata from the filesystem.
func (daemon *Daemon) cleanupContainer(container *container.Container, config types.ContainerRmConfig) error {
	logrus.Debugf("cleanupContainer: %s id", container.ID)
	if container.IsRunning() {
		if !config.ForceRemove {
			state := container.StateString()
			procedure := "Stop the container before attempting removal or force remove"
			if state == "paused" {
				procedure = "Unpause and then " + strings.ToLower(procedure)
			}
			err := fmt.Errorf("You cannot remove a %s container %s. %s", state, container.ID, procedure)
			return errdefs.Conflict(err)
		}
		if err := daemon.Kill(container); err != nil {
			return fmt.Errorf("Could not kill running container %s, cannot remove - %v", container.ID, err)
		}
	}

	// stop collection of stats for the container regardless
	// if stats are currently getting collected.
	daemon.statsCollector.StopCollection(container)

	// stopTimeout is the number of seconds to wait for the container to stop
	// gracefully before forcibly killing it.
	//
	// Why 3 seconds? The timeout specified here was originally added in commit
	// 1615bb08c7c3fc6c4b22db0a633edda516f97cf0, which added a custom timeout to
	// some commands, but lacking an option for a timeout on "docker rm", was
	// hardcoded to 10 seconds. Commit 28fd289b448164b77affd8103c0d96fd8110daf9
	// later on updated this to 3 seconds (but no background on that change).
	//
	// If you arrived here and know the answer, you earned yourself a picture
	// of a cute animal of your own choosing.
	logrus.Debugf("daemon.containerStop: %s timeout 3", container.ID)
	var stopTimeout = 3
	if err := daemon.containerStop(context.TODO(), container, containertypes.StopOptions{Timeout: &stopTimeout}); err != nil {
		logrus.Debugf("daemon.containerStop id %s error %s", container.ID, err.Error())
		return err
	}

	// Mark container dead. We don't want anybody to be restarting it.
	container.Lock()
	logrus.Debugf("daemon.cleanupContainer Lock: id %s", container.ID)
	container.Dead = true

	// Save container state to disk. So that if error happens before
	// container meta file got removed from disk, then a restart of
	// docker should not make a dead container alive.
	logrus.Debugf("container.CheckpointTo: id %s", container.ID)
	if err := container.CheckpointTo(daemon.containersReplica); err != nil && !os.IsNotExist(err) {
		logrus.Errorf("Error saving dying container to disk: %v", err)
	}
	container.Unlock()
	logrus.Debugf("daemon.cleanupContainer Unlock: id %s", container.ID)

	// When container creation fails and `RWLayer` has not been created yet, we
	// do not call `ReleaseRWLayer`
	if container.RWLayer != nil {
		if err := daemon.imageService.ReleaseLayer(container.RWLayer); err != nil {
			err = errors.Wrapf(err, "container %s", container.ID)
			container.SetRemovalError(err)
			return err
		}
		container.RWLayer = nil
	} else {
		if daemon.UsesSnapshotter() {
			ls := daemon.containerdCli.LeasesService()
			lease := leases.Lease{
				ID: container.ID,
			}
			if err := ls.Delete(context.Background(), lease, leases.SynchronousDelete); err != nil {
				container.SetRemovalError(err)
				return err
			}
		}
	}

	// Hold the container lock while deleting the container root directory
	// so that other goroutines don't attempt to concurrently open files
	// within it. Having any file open on Windows (without the
	// FILE_SHARE_DELETE flag) will block it from being deleted.
	container.Lock()
	logrus.Debugf("daemon.cleanupContainer Lock2: id %s", container.ID)
	logrus.Debugf("containersfs.EnsureRemoveAll: id %s root %s", container.ID, container.Root)
	err := containerfs.EnsureRemoveAll(container.Root)
	container.Unlock()
	logrus.Debugf("daemon.cleanupContainer UnLock2: id %s", container.ID)
	if err != nil {
		err = errors.Wrapf(err, "unable to remove filesystem for %s", container.ID)
		container.SetRemovalError(err)
		return err
	}

	linkNames := daemon.linkIndex.delete(container)
	selinux.ReleaseLabel(container.ProcessLabel)
	daemon.containers.Delete(container.ID)
	daemon.containersReplica.Delete(container)
	logrus.Debugf("daemon.removeMountPoints: id %s ", container.ID)
	if err := daemon.removeMountPoints(container, config.RemoveVolume); err != nil {
		logrus.Error(err)
	}
	for _, name := range linkNames {
		daemon.releaseName(name)
	}
	container.SetRemoved()
	stateCtr.del(container.ID)

	daemon.LogContainerEvent(container, "destroy")
	return nil
}

func (daemon *Daemon) onlyCleanupContainer(container *container.Container, config types.ContainerRmConfig, force bool) error {

	daemon.statsCollector.OnlyStopCollection(container)

	container.Dead = true
	logrus.Debugf("onlyCleanupContainer.CheckpointTo: id %s", container.ID)
	if err := container.CheckpointTo(daemon.containersReplica); err != nil && !os.IsNotExist(err) {
		logrus.Errorf("Error saving dying container to disk: %v", err)
	}

	if !force {
		if container.RWLayer != nil {
			logrus.Debugf("daemon.imageService.ReleaseLayer: id %s", container.ID)
			if err := daemon.imageService.ReleaseLayer(container.RWLayer); err != nil {
				err = errors.Wrapf(err, "container %s", container.ID)
				container.OnlySetRemovalError(err)
				logrus.Debugf("daemon.imageService.ReleaseLayer: id %s err %s", container.ID, err.Error())
				//return err
			}
			container.RWLayer = nil
		} else {
			if daemon.UsesSnapshotter() {
				logrus.Debugf("daemon.UsesSnapshotter: id %s", container.ID)
				ls := daemon.containerdCli.LeasesService()
				lease := leases.Lease{
					ID: container.ID,
				}
				if err := ls.Delete(context.Background(), lease, leases.SynchronousDelete); err != nil {
					container.OnlySetRemovalError(err)
					logrus.Debugf("daemon.UsesSnapshotter: id %s err %s", container.ID, err.Error())
					//return err
				}
			}
		}
	}

	logrus.Debugf("daemon.EnsureRemoveAll: id %s", container.ID)
	err := containerfs.EnsureRemoveAll(container.Root)
	if err != nil {
		log.Errorf("remote container id %s fs error %s ", container.ID, err.Error())
	}

	linkNames := daemon.linkIndex.onlyDelete(container)
	selinux.ReleaseLabel(container.ProcessLabel)
	daemon.containers.Delete(container.ID)
	daemon.containersReplica.Delete(container)
	logrus.Debugf("daemon.removeMountPoints: id %s ", container.ID)
	if err := daemon.removeMountPoints(container, config.RemoveVolume); err != nil {
		logrus.Error(err)
	}
	for _, name := range linkNames {
		daemon.releaseName(name)
	}
	container.OnlySetRemovalError(nil)
	stateCtr.OnlyDel(container.ID)

	go daemon.LogContainerEvent(container, "destroy")

	return nil
}

func pidExists(pid int) bool {
	logrus.Debugf("pidExists: pid %d", pid)
	if pid%4 != 0 {
		var ret []int32
		var read uint32 = 0
		var psSize uint32 = 1024
		const dwordSize uint32 = 4

		for {
			ps := make([]uint32, psSize)
			if err := windows.EnumProcesses(ps, &read); err != nil {
				logrus.Debugf("windows.EnumProcesses: err %s", err.Error())
				break
			}
			if uint32(len(ps)) == read { // ps buffer was too small to host every results, retry with a bigger one
				psSize += 1024
				continue
			}
			for _, pid := range ps[:read/dwordSize] {
				ret = append(ret, int32(pid))
			}
			break
		}
		if len(ret) > 0 {
			for _, i := range ret {
				if i == int32(pid) {
					return true
				}
			}
			return false
		}
	}
	logrus.Debugf("pidExists2: pid %d", pid)
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err == windows.ERROR_ACCESS_DENIED {
		return true
	}
	if err == windows.ERROR_INVALID_PARAMETER {
		return false
	}
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	logrus.Debugf("pidExists3: pid %d", pid)
	event, err := windows.WaitForSingleObject(h, 0)
	log.Debugf("pidExists4: pid %d event %d", pid, event)
	return event == uint32(windows.WAIT_TIMEOUT)
}

func pidList() []int {
	var ret []int
	var read uint32 = 0
	var psSize uint32 = 1024
	const dwordSize uint32 = 4

	for {
		ps := make([]uint32, psSize)
		if err := windows.EnumProcesses(ps, &read); err != nil {
			fmt.Println(err.Error())
			return ret
		}
		if uint32(len(ps)) == read { // ps buffer was too small to host every results, retry with a bigger one
			psSize += 1024
			continue
		}
		for _, pid := range ps[:read/dwordSize] {
			ret = append(ret, int(pid))
		}
		break
	}
	return ret
}

func pidExist2(pid int, pids []int) bool {
	for _, p := range pids {
		if pid == p {
			return true
		}
	}
	return false
}

// OnlyContainerRm removes the container id from the filesystem. An error
// is returned if the container is not found, or if the remove
// fails. If the remove succeeds, the container name is released, and
// network links are removed.
func (daemon *Daemon) OnlyContainerRm(name string, config *types.ContainerRmConfig, force bool) error {
	logrus.Debugf("daemon.ContainerRm: name %s", name)
	start := time.Now()
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		logrus.Debugf("daemon.ContainerRm: id %s err %s", name, err.Error())
		return nil
	}
	if force {
		logrus.Debugf("onlyCleanupContainer force delete: id %s pid %d", ctr.ID, ctr.Pid)
		err = daemon.onlyCleanupContainer(ctr, *config, force)
		if err != nil {
			logrus.Debugf("onlyCleanupContainer: id %s err %s", ctr.ID, err.Error())
		}
		containerActions.WithValues("delete").UpdateSince(start)
		return nil
	}
	if ctr.Pid == 0 {
		logrus.Debugf("onlyCleanupContainer1: id %s pid %d", ctr.ID, ctr.Pid)
		err = daemon.onlyCleanupContainer(ctr, *config, true)
		if err != nil {
			logrus.Debugf("onlyCleanupContainer1: id %s err %s", ctr.ID, err.Error())
		}
		containerActions.WithValues("delete").UpdateSince(start)
		return nil
	}
	return nil
}
