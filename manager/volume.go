package manager

import (
	"fmt"
	"sync"

	"github.com/pkg/errors"

	"github.com/yasker/lm-rewrite/types"
	"github.com/yasker/lm-rewrite/util"
)

func (m *VolumeManager) NewVolume(info *types.VolumeInfo) error {
	if info.Name == "" || info.Size == 0 || info.NumberOfReplicas == 0 {
		return fmt.Errorf("missing required parameter %+v", info)
	}
	vol, err := m.kv.GetVolume(info.Name)
	if err != nil {
		return err
	}
	if vol != nil {
		return fmt.Errorf("volume %v already exists", info.Name)
	}

	if err := m.kv.CreateVolume(info); err != nil {
		return err
	}

	return nil
}

func (m *VolumeManager) GetVolume(volumeName string) (*Volume, error) {
	if volumeName == "" {
		return nil, fmt.Errorf("invalid empty volume name")
	}

	info, err := m.kv.GetVolume(volumeName)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, fmt.Errorf("cannot find volume %v", volumeName)
	}
	controller, err := m.kv.GetVolumeController(volumeName)
	if err != nil {
		return nil, err
	}
	replicas, err := m.kv.ListVolumeReplicas(volumeName)
	if err != nil {
		return nil, err
	}
	if replicas == nil {
		replicas = make(map[string]*types.ReplicaInfo)
	}

	return &Volume{
		VolumeInfo: *info,
		Controller: controller,
		Replicas:   replicas,
	}, nil
}

func (m *VolumeManager) getManagedVolume(volumeName string) (*ManagedVolume, error) {
	volume, err := m.GetVolume(volumeName)
	if err != nil {
		return nil, err
	}
	return &ManagedVolume{
		Volume: *volume,
		mutex:  &sync.RWMutex{},
		Jobs:   map[string]*Job{},
		m:      m,
	}, nil
}

func (v *ManagedVolume) badReplicaCounts() int {
	count := 0
	for _, replica := range v.Replicas {
		if replica.FailedAt != "" {
			count++
		}
	}
	return count
}

func (v *ManagedVolume) Cleanup() (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "cannot cleanup stale replicas")
		}
	}()

	staleReplicas := map[string]*types.ReplicaInfo{}

	for _, replica := range v.Replicas {
		if replica.FailedAt != "" {
			if util.TimestampAfterTimeout(replica.FailedAt, v.StaleReplicaTimeout*60) {
				staleReplicas[replica.Name] = replica
			}
		}
	}

	for _, replica := range staleReplicas {
		if err := v.deleteReplica(replica.Name); err != nil {
			return err
		}
	}
	return nil
}

func (v *ManagedVolume) create() (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "cannot create volume")
		}
	}()

	ready := 0
	for _, replica := range v.Replicas {
		if replica.FailedAt != "" {
			ready++
		}
	}
	nodesWithReplica := v.getNodesWithReplica()

	creatingJobs := v.listOngoingJobsByType(JobTypeReplicaCreate)
	creating := len(creatingJobs)
	for _, job := range creatingJobs {
		data := job.Data
		if data["NodeID"] != "" {
			nodesWithReplica[data["NodeID"]] = struct{}{}
		}
	}

	for i := 0; i < v.NumberOfReplicas-creating-ready; i++ {
		nodeID, err := v.m.ScheduleReplica(&v.VolumeInfo, nodesWithReplica)
		if err != nil {
			return err
		}

		if err := v.createReplica(nodeID); err != nil {
			return err
		}
		nodesWithReplica[nodeID] = struct{}{}
	}
	return nil
}

func (v *ManagedVolume) start() (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "cannot start volume")
		}
	}()

	startReplicas := map[string]*types.ReplicaInfo{}
	for _, replica := range v.Replicas {
		if replica.FailedAt != "" {
			continue
		}
		if err := v.startReplica(replica.Name); err != nil {
			return err
		}
		startReplicas[replica.Name] = replica
	}
	if len(startReplicas) == 0 {
		return fmt.Errorf("cannot start with no replicas")
	}
	if v.Controller == nil {
		if err := v.createController(startReplicas); err != nil {
			return err
		}
	}
	return nil
}

func (v *ManagedVolume) stop() (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "cannot stop volume")
		}
	}()

	if err := v.stopRebuild(); err != nil {
		return err
	}
	if v.Controller != nil {
		if err := v.deleteController(); err != nil {
			return err
		}
	}
	if v.Replicas != nil {
		for name := range v.Replicas {
			if err := v.stopReplica(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *ManagedVolume) heal() (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "cannot heal volume")
		}
	}()

	if v.Controller == nil {
		return fmt.Errorf("cannot heal without controller")
	}
	if err := v.startRebuild(); err != nil {
		return err
	}
	return nil
}

func (v *ManagedVolume) destroy() (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "cannot destroy volume")
		}
	}()
	if err := v.stop(); err != nil {
		return err
	}
	if v.Replicas != nil {
		for name := range v.Replicas {
			if err := v.deleteReplica(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *ManagedVolume) getNodesWithReplica() map[string]struct{} {
	ret := map[string]struct{}{}

	v.mutex.RLock()
	defer v.mutex.RUnlock()
	for _, replica := range v.Replicas {
		if replica.FailedAt != "" {
			ret[replica.NodeID] = struct{}{}
		}
	}
	return ret
}

func (v *ManagedVolume) SnapshotPurge() error {
	purgingJobs := v.listOngoingJobsByType(JobTypeSnapshotPurge)
	if len(purgingJobs) != 0 {
		return nil
	}

	errCh := make(chan error)
	go func() {
		errCh <- v.jobSnapshotPurge()
	}()

	if _, err := v.registerJob(JobTypeSnapshotPurge, v.Name, nil, errCh); err != nil {
		return err
	}
	return nil
}

func (v *ManagedVolume) SnapshotBackup(snapName, backupTarget string) error {
	backupJobs := v.listOngoingJobsByTypeAndAssociateID(JobTypeSnapshotBackup, snapName)
	if len(backupJobs) != 0 {
		return nil
	}

	errCh := make(chan error)
	go func() {
		errCh <- v.jobSnapshotBackup(snapName, backupTarget)
	}()

	if _, err := v.registerJob(JobTypeSnapshotBackup, snapName, nil, errCh); err != nil {
		return err
	}
	return nil
}
