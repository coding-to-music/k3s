/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package local

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"k8s.io/klog"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/validation"
	"k8s.io/utils/keymutex"
	utilstrings "k8s.io/utils/strings"
)

const (
	defaultFSType = "ext4"
)

// This is the primary entrypoint for volume plugins.
func ProbeVolumePlugins() []volume.VolumePlugin {
	return []volume.VolumePlugin{&localVolumePlugin{}}
}

type localVolumePlugin struct {
	host        volume.VolumeHost
	volumeLocks keymutex.KeyMutex
	recorder    record.EventRecorder
}

var _ volume.VolumePlugin = &localVolumePlugin{}
var _ volume.PersistentVolumePlugin = &localVolumePlugin{}
var _ volume.BlockVolumePlugin = &localVolumePlugin{}

const (
	localVolumePluginName = "kubernetes.io/local-volume"
)

func (plugin *localVolumePlugin) Init(host volume.VolumeHost) error {
	plugin.host = host
	plugin.volumeLocks = keymutex.NewHashed(0)
	plugin.recorder = host.GetEventRecorder()
	return nil
}

func (plugin *localVolumePlugin) GetPluginName() string {
	return localVolumePluginName
}

func (plugin *localVolumePlugin) GetVolumeName(spec *volume.Spec) (string, error) {
	// This volume is only supported as a PersistentVolumeSource, so the PV name is unique
	return spec.Name(), nil
}

func (plugin *localVolumePlugin) CanSupport(spec *volume.Spec) bool {
	// This volume is only supported as a PersistentVolumeSource
	return (spec.PersistentVolume != nil && spec.PersistentVolume.Spec.Local != nil)
}

func (plugin *localVolumePlugin) IsMigratedToCSI() bool {
	return false
}

func (plugin *localVolumePlugin) RequiresRemount() bool {
	return false
}

func (plugin *localVolumePlugin) SupportsMountOption() bool {
	return true
}

func (plugin *localVolumePlugin) SupportsBulkVolumeVerification() bool {
	return false
}

func (plugin *localVolumePlugin) GetAccessModes() []v1.PersistentVolumeAccessMode {
	// The current meaning of AccessMode is how many nodes can attach to it, not how many pods can mount it
	return []v1.PersistentVolumeAccessMode{
		v1.ReadWriteOnce,
	}
}

func getVolumeSource(spec *volume.Spec) (*v1.LocalVolumeSource, bool, error) {
	if spec.PersistentVolume != nil && spec.PersistentVolume.Spec.Local != nil {
		return spec.PersistentVolume.Spec.Local, spec.ReadOnly, nil
	}

	return nil, false, fmt.Errorf("Spec does not reference a Local volume type")
}

func (plugin *localVolumePlugin) NewMounter(spec *volume.Spec, pod *v1.Pod, _ volume.VolumeOptions) (volume.Mounter, error) {
	_, readOnly, err := getVolumeSource(spec)
	if err != nil {
		return nil, err
	}

	globalLocalPath, err := plugin.getGlobalLocalPath(spec)
	if err != nil {
		return nil, err
	}

	return &localVolumeMounter{
		localVolume: &localVolume{
			pod:             pod,
			podUID:          pod.UID,
			volName:         spec.Name(),
			mounter:         plugin.host.GetMounter(plugin.GetPluginName()),
			plugin:          plugin,
			globalPath:      globalLocalPath,
			MetricsProvider: volume.NewMetricsStatFS(plugin.host.GetPodVolumeDir(pod.UID, utilstrings.EscapeQualifiedName(localVolumePluginName), spec.Name())),
		},
		mountOptions: util.MountOptionFromSpec(spec),
		readOnly:     readOnly,
	}, nil

}

func (plugin *localVolumePlugin) NewUnmounter(volName string, podUID types.UID) (volume.Unmounter, error) {
	return &localVolumeUnmounter{
		localVolume: &localVolume{
			podUID:  podUID,
			volName: volName,
			mounter: plugin.host.GetMounter(plugin.GetPluginName()),
			plugin:  plugin,
		},
	}, nil
}

func (plugin *localVolumePlugin) NewBlockVolumeMapper(spec *volume.Spec, pod *v1.Pod,
	_ volume.VolumeOptions) (volume.BlockVolumeMapper, error) {
	volumeSource, readOnly, err := getVolumeSource(spec)
	if err != nil {
		return nil, err
	}

	return &localVolumeMapper{
		localVolume: &localVolume{
			podUID:     pod.UID,
			volName:    spec.Name(),
			globalPath: volumeSource.Path,
			plugin:     plugin,
		},
		readOnly: readOnly,
	}, nil

}

func (plugin *localVolumePlugin) NewBlockVolumeUnmapper(volName string,
	podUID types.UID) (volume.BlockVolumeUnmapper, error) {
	return &localVolumeUnmapper{
		localVolume: &localVolume{
			podUID:  podUID,
			volName: volName,
			plugin:  plugin,
		},
	}, nil
}

// TODO: check if no path and no topology constraints are ok
func (plugin *localVolumePlugin) ConstructVolumeSpec(volumeName, mountPath string) (*volume.Spec, error) {
	fs := v1.PersistentVolumeFilesystem
	localVolume := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: volumeName,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				Local: &v1.LocalVolumeSource{
					Path: "",
				},
			},
			VolumeMode: &fs,
		},
	}
	return volume.NewSpecFromPersistentVolume(localVolume, false), nil
}

func (plugin *localVolumePlugin) ConstructBlockVolumeSpec(podUID types.UID, volumeName,
	mapPath string) (*volume.Spec, error) {
	block := v1.PersistentVolumeBlock

	localVolume := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: volumeName,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				Local: &v1.LocalVolumeSource{
					Path: "",
				},
			},
			VolumeMode: &block,
		},
	}

	return volume.NewSpecFromPersistentVolume(localVolume, false), nil
}

func (plugin *localVolumePlugin) generateBlockDeviceBaseGlobalPath() string {
	return filepath.Join(plugin.host.GetPluginDir(localVolumePluginName), mount.MountsInGlobalPDPath)
}

func (plugin *localVolumePlugin) getGlobalLocalPath(spec *volume.Spec) (string, error) {
	if spec.PersistentVolume.Spec.Local == nil || len(spec.PersistentVolume.Spec.Local.Path) == 0 {
		return "", fmt.Errorf("local volume source is nil or local path is not set")
	}

	fileType, err := plugin.host.GetMounter(plugin.GetPluginName()).GetFileType(spec.PersistentVolume.Spec.Local.Path)
	if err != nil {
		return "", err
	}
	switch fileType {
	case mount.FileTypeDirectory:
		return spec.PersistentVolume.Spec.Local.Path, nil
	case mount.FileTypeBlockDev:
		return filepath.Join(plugin.generateBlockDeviceBaseGlobalPath(), spec.Name()), nil
	default:
		return "", fmt.Errorf("only directory and block device are supported")
	}
}

var _ volume.DeviceMountableVolumePlugin = &localVolumePlugin{}

type deviceMounter struct {
	plugin  *localVolumePlugin
	mounter *mount.SafeFormatAndMount
}

var _ volume.DeviceMounter = &deviceMounter{}

func (plugin *localVolumePlugin) NewDeviceMounter() (volume.DeviceMounter, error) {
	return &deviceMounter{
		plugin:  plugin,
		mounter: util.NewSafeFormatAndMountFromHost(plugin.GetPluginName(), plugin.host),
	}, nil
}

func (dm *deviceMounter) mountLocalBlockDevice(spec *volume.Spec, devicePath string, deviceMountPath string) error {
	klog.V(4).Infof("local: mounting device %s to %s", devicePath, deviceMountPath)
	notMnt, err := dm.mounter.IsLikelyNotMountPoint(deviceMountPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(deviceMountPath, 0750); err != nil {
				return err
			}
			notMnt = true
		} else {
			return err
		}
	}
	if !notMnt {
		return nil
	}
	fstype, err := getVolumeSourceFSType(spec)
	if err != nil {
		return err
	}

	ro, err := getVolumeSourceReadOnly(spec)
	if err != nil {
		return err
	}
	options := []string{}
	if ro {
		options = append(options, "ro")
	}
	mountOptions := util.MountOptionFromSpec(spec, options...)
	err = dm.mounter.FormatAndMount(devicePath, deviceMountPath, fstype, mountOptions)
	if err != nil {
		os.Remove(deviceMountPath)
		return fmt.Errorf("local: failed to mount device %s at %s (fstype: %s), error %v", devicePath, deviceMountPath, fstype, err)
	}
	klog.V(3).Infof("local: successfully mount device %s at %s (fstype: %s)", devicePath, deviceMountPath, fstype)
	return nil
}

func (dm *deviceMounter) MountDevice(spec *volume.Spec, devicePath string, deviceMountPath string) error {
	if spec.PersistentVolume.Spec.Local == nil || len(spec.PersistentVolume.Spec.Local.Path) == 0 {
		return fmt.Errorf("local volume source is nil or local path is not set")
	}
	fileType, err := dm.mounter.GetFileType(spec.PersistentVolume.Spec.Local.Path)
	if err != nil {
		return err
	}

	switch fileType {
	case mount.FileTypeBlockDev:
		// local volume plugin does not implement AttachableVolumePlugin interface, so set devicePath to Path in PV spec directly
		devicePath = spec.PersistentVolume.Spec.Local.Path
		return dm.mountLocalBlockDevice(spec, devicePath, deviceMountPath)
	case mount.FileTypeDirectory:
		// if the given local volume path is of already filesystem directory, return directly
		return nil
	default:
		return fmt.Errorf("only directory and block device are supported")
	}
}

func getVolumeSourceFSType(spec *volume.Spec) (string, error) {
	if spec.PersistentVolume != nil &&
		spec.PersistentVolume.Spec.Local != nil {
		if spec.PersistentVolume.Spec.Local.FSType != nil {
			return *spec.PersistentVolume.Spec.Local.FSType, nil
		}
		// if the FSType is not set in local PV spec, setting it to default ("ext4")
		return defaultFSType, nil
	}

	return "", fmt.Errorf("spec does not reference a Local volume type")
}

func getVolumeSourceReadOnly(spec *volume.Spec) (bool, error) {
	if spec.PersistentVolume != nil &&
		spec.PersistentVolume.Spec.Local != nil {
		// local volumes used as a PersistentVolume gets the ReadOnly flag indirectly through
		// the persistent-claim volume used to mount the PV
		return spec.ReadOnly, nil
	}

	return false, fmt.Errorf("spec does not reference a Local volume type")
}

func (dm *deviceMounter) GetDeviceMountPath(spec *volume.Spec) (string, error) {
	return dm.plugin.getGlobalLocalPath(spec)
}

func (plugin *localVolumePlugin) NewDeviceUnmounter() (volume.DeviceUnmounter, error) {
	return &deviceMounter{
		plugin:  plugin,
		mounter: util.NewSafeFormatAndMountFromHost(plugin.GetPluginName(), plugin.host),
	}, nil
}

func (plugin *localVolumePlugin) GetDeviceMountRefs(deviceMountPath string) ([]string, error) {
	mounter := plugin.host.GetMounter(plugin.GetPluginName())
	return mounter.GetMountRefs(deviceMountPath)
}

var _ volume.DeviceUnmounter = &deviceMounter{}

func (dm *deviceMounter) UnmountDevice(deviceMountPath string) error {
	// If the local PV is a block device,
	// The deviceMountPath is generated to the format like :/var/lib/kubelet/plugins/kubernetes.io/local-volume/mounts/localpv.spec.Name;
	// If it is a filesystem directory, then the deviceMountPath is set directly to pvSpec.Local.Path
	// We only need to unmount block device here, so we need to check if the deviceMountPath passed here
	// has base mount path: /var/lib/kubelet/plugins/kubernetes.io/local-volume/mounts
	basemountPath := dm.plugin.generateBlockDeviceBaseGlobalPath()
	if mount.PathWithinBase(deviceMountPath, basemountPath) {
		return mount.CleanupMountPoint(deviceMountPath, dm.mounter, false)
	}

	return nil
}

// Local volumes represent a local directory on a node.
// The directory at the globalPath will be bind-mounted to the pod's directory
type localVolume struct {
	volName string
	pod     *v1.Pod
	podUID  types.UID
	// Global path to the volume
	globalPath string
	// Mounter interface that provides system calls to mount the global path to the pod local path.
	mounter mount.Interface
	plugin  *localVolumePlugin
	volume.MetricsProvider
}

func (l *localVolume) GetPath() string {
	return l.plugin.host.GetPodVolumeDir(l.podUID, utilstrings.EscapeQualifiedName(localVolumePluginName), l.volName)
}

type localVolumeMounter struct {
	*localVolume
	readOnly     bool
	mountOptions []string
}

var _ volume.Mounter = &localVolumeMounter{}

func (m *localVolumeMounter) GetAttributes() volume.Attributes {
	return volume.Attributes{
		ReadOnly:        m.readOnly,
		Managed:         !m.readOnly,
		SupportsSELinux: true,
	}
}

// CanMount checks prior to mount operations to verify that the required components (binaries, etc.)
// to mount the volume are available on the underlying node.
// If not, it returns an error
func (m *localVolumeMounter) CanMount() error {
	return nil
}

// SetUp bind mounts the directory to the volume path
func (m *localVolumeMounter) SetUp(fsGroup *int64) error {
	return m.SetUpAt(m.GetPath(), fsGroup)
}

// SetUpAt bind mounts the directory to the volume path and sets up volume ownership
func (m *localVolumeMounter) SetUpAt(dir string, fsGroup *int64) error {
	m.plugin.volumeLocks.LockKey(m.globalPath)
	defer m.plugin.volumeLocks.UnlockKey(m.globalPath)

	if m.globalPath == "" {
		return fmt.Errorf("LocalVolume volume %q path is empty", m.volName)
	}

	err := validation.ValidatePathNoBacksteps(m.globalPath)
	if err != nil {
		return fmt.Errorf("invalid path: %s %v", m.globalPath, err)
	}

	notMnt, err := m.mounter.IsNotMountPoint(dir)
	klog.V(4).Infof("LocalVolume mount setup: PodDir(%s) VolDir(%s) Mounted(%t) Error(%v), ReadOnly(%t)", dir, m.globalPath, !notMnt, err, m.readOnly)
	if err != nil && !os.IsNotExist(err) {
		klog.Errorf("cannot validate mount point: %s %v", dir, err)
		return err
	}

	if !notMnt {
		return nil
	}
	refs, err := m.mounter.GetMountRefs(m.globalPath)
	if fsGroup != nil {
		if err != nil {
			klog.Errorf("cannot collect mounting information: %s %v", m.globalPath, err)
			return err
		}

		// Only count mounts from other pods
		refs = m.filterPodMounts(refs)
		if len(refs) > 0 {
			fsGroupNew := int64(*fsGroup)
			fsGroupOld, err := m.mounter.GetFSGroup(m.globalPath)
			if err != nil {
				return fmt.Errorf("failed to check fsGroup for %s (%v)", m.globalPath, err)
			}
			if fsGroupNew != fsGroupOld {
				m.plugin.recorder.Eventf(m.pod, v1.EventTypeWarning, events.WarnAlreadyMountedVolume, "The requested fsGroup is %d, but the volume %s has GID %d. The volume may not be shareable.", fsGroupNew, m.volName, fsGroupOld)
			}
		}

	}

	if runtime.GOOS != "windows" {
		// skip below MkdirAll for windows since the "bind mount" logic is implemented differently in mount_wiondows.go
		if err := os.MkdirAll(dir, 0750); err != nil {
			klog.Errorf("mkdir failed on disk %s (%v)", dir, err)
			return err
		}
	}
	// Perform a bind mount to the full path to allow duplicate mounts of the same volume.
	options := []string{"bind"}
	if m.readOnly {
		options = append(options, "ro")
	}
	mountOptions := util.JoinMountOptions(options, m.mountOptions)

	klog.V(4).Infof("attempting to mount %s", dir)
	globalPath := util.MakeAbsolutePath(runtime.GOOS, m.globalPath)
	err = m.mounter.Mount(globalPath, dir, "", mountOptions)
	if err != nil {
		klog.Errorf("Mount of volume %s failed: %v", dir, err)
		notMnt, mntErr := m.mounter.IsNotMountPoint(dir)
		if mntErr != nil {
			klog.Errorf("IsNotMountPoint check failed: %v", mntErr)
			return err
		}
		if !notMnt {
			if mntErr = m.mounter.Unmount(dir); mntErr != nil {
				klog.Errorf("Failed to unmount: %v", mntErr)
				return err
			}
			notMnt, mntErr = m.mounter.IsNotMountPoint(dir)
			if mntErr != nil {
				klog.Errorf("IsNotMountPoint check failed: %v", mntErr)
				return err
			}
			if !notMnt {
				// This is very odd, we don't expect it.  We'll try again next sync loop.
				klog.Errorf("%s is still mounted, despite call to unmount().  Will try again next sync loop.", dir)
				return err
			}
		}
		os.Remove(dir)
		return err
	}
	if !m.readOnly {
		// Volume owner will be written only once on the first volume mount
		if len(refs) == 0 {
			return volume.SetVolumeOwnership(m, fsGroup)
		}
	}
	return nil
}

// filterPodMounts only returns mount paths inside the kubelet pod directory
func (m *localVolumeMounter) filterPodMounts(refs []string) []string {
	filtered := []string{}
	for _, r := range refs {
		if strings.HasPrefix(r, m.plugin.host.GetPodsDir()+string(os.PathSeparator)) {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

type localVolumeUnmounter struct {
	*localVolume
}

var _ volume.Unmounter = &localVolumeUnmounter{}

// TearDown unmounts the bind mount
func (u *localVolumeUnmounter) TearDown() error {
	return u.TearDownAt(u.GetPath())
}

// TearDownAt unmounts the bind mount
func (u *localVolumeUnmounter) TearDownAt(dir string) error {
	klog.V(4).Infof("Unmounting volume %q at path %q\n", u.volName, dir)
	return mount.CleanupMountPoint(dir, u.mounter, true) /* extensiveMountPointCheck = true */
}

// localVolumeMapper implements the BlockVolumeMapper interface for local volumes.
type localVolumeMapper struct {
	*localVolume
	readOnly bool
}

var _ volume.BlockVolumeMapper = &localVolumeMapper{}

// SetUpDevice provides physical device path for the local PV.
func (m *localVolumeMapper) SetUpDevice() (string, error) {
	globalPath := util.MakeAbsolutePath(runtime.GOOS, m.globalPath)
	klog.V(4).Infof("SetupDevice returning path %s", globalPath)
	return globalPath, nil
}

func (m *localVolumeMapper) MapDevice(devicePath, globalMapPath, volumeMapPath, volumeMapName string, podUID types.UID) error {
	return util.MapBlockVolume(devicePath, globalMapPath, volumeMapPath, volumeMapName, podUID)
}

// localVolumeUnmapper implements the BlockVolumeUnmapper interface for local volumes.
type localVolumeUnmapper struct {
	*localVolume
}

var _ volume.BlockVolumeUnmapper = &localVolumeUnmapper{}

// TearDownDevice will undo SetUpDevice procedure. In local PV, all of this already handled by operation_generator.
func (u *localVolumeUnmapper) TearDownDevice(mapPath, _ string) error {
	klog.V(4).Infof("local: TearDownDevice completed for: %s", mapPath)
	return nil
}

// GetGlobalMapPath returns global map path and error.
// path: plugins/kubernetes.io/kubernetes.io/local-volume/volumeDevices/{volumeName}
func (lv *localVolume) GetGlobalMapPath(spec *volume.Spec) (string, error) {
	return filepath.Join(lv.plugin.host.GetVolumeDevicePluginDir(utilstrings.EscapeQualifiedName(localVolumePluginName)),
		lv.volName), nil
}

// GetPodDeviceMapPath returns pod device map path and volume name.
// path: pods/{podUid}/volumeDevices/kubernetes.io~local-volume
// volName: local-pv-ff0d6d4
func (lv *localVolume) GetPodDeviceMapPath() (string, string) {
	return lv.plugin.host.GetPodVolumeDeviceDir(lv.podUID,
		utilstrings.EscapeQualifiedName(localVolumePluginName)), lv.volName
}