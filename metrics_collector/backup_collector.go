package metricscollector

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	"github.com/longhorn/longhorn-manager/datastore"
	"github.com/longhorn/longhorn-manager/types"
)

type BackupCollector struct {
	*baseCollector

	sizeMetric  metricInfo
	stateMetric metricInfo
}

func NewBackupCollector(
	logger logrus.FieldLogger,
	nodeID string,
	ds *datastore.DataStore) *BackupCollector {

	vc := &BackupCollector{
		baseCollector: newBaseCollector(subsystemBackup, logger, nodeID, ds),
	}

	vc.sizeMetric = metricInfo{
		Desc: prometheus.NewDesc(
			prometheus.BuildFQName(longhornName, subsystemBackup, "actual_size_bytes"),
			"Actual size of this backup",
			[]string{volumeLabel, backupLabel},
			nil,
		),
		Type: prometheus.GaugeValue,
	}

	vc.stateMetric = metricInfo{
		Desc: prometheus.NewDesc(
			prometheus.BuildFQName(longhornName, subsystemBackup, "state"),
			"State of this backup",
			[]string{volumeLabel, backupLabel, stateLabel},
			nil,
		),
		Type: prometheus.GaugeValue,
	}

	return vc
}

func (vc *BackupCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- vc.sizeMetric.Desc
	ch <- vc.stateMetric.Desc
}

func (vc *BackupCollector) Collect(ch chan<- prometheus.Metric) {
	defer func() {
		if err := recover(); err != nil {
			vc.logger.WithField("error", err).Warn("Panic during collecting metrics")
		}
	}()

	backupLists, err := vc.ds.ListBackupsRO()
	if err != nil {
		vc.logger.WithError(err).Warn("Error during scrape")
		return
	}

	for _, v := range backupLists {
		if v.Status.OwnerID == vc.currentNodeID {
			var size float64
			if size, err = strconv.ParseFloat(v.Status.Size, 64); err != nil {
				vc.logger.WithError(err).Warn("Error get size")
			}
			backupVolumeName, ok := v.Labels[types.LonghornLabelBackupVolume]
			if !ok {
				vc.logger.WithError(err).Warn("Error get backup volume label")
			}
			ch <- prometheus.MustNewConstMetric(vc.sizeMetric.Desc, vc.sizeMetric.Type, size, backupVolumeName, v.Name)
			ch <- prometheus.MustNewConstMetric(vc.stateMetric.Desc, vc.stateMetric.Type, 1, backupVolumeName, v.Name, string(v.Status.State))
		}
	}
}
