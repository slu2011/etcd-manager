package backupcontroller

import (
	"context"
	"fmt"
	"time"

	"github.com/golang/glog"

	protoetcd "kope.io/etcd-manager/pkg/apis/etcd"
	"kope.io/etcd-manager/pkg/backup"
	"kope.io/etcd-manager/pkg/contextutil"
	"kope.io/etcd-manager/pkg/etcd"
	"kope.io/etcd-manager/pkg/etcdclient"
)

const loopInterval = time.Minute

type BackupController struct {
	clusterName string
	backupStore backup.Store

	dataDir string

	clientUrls  []string
	etcdVersion string

	// lastBackup is the time at which we last performed a backup (as leader)
	lastBackup time.Time

	backupInterval time.Duration

	backupCleanup *BackupCleanup
}

func NewBackupController(backupStore backup.Store, clusterName string, clientUrls []string, etcdVersion string, dataDir string) (*BackupController, error) {
	if clusterName == "" {
		return nil, fmt.Errorf("ClusterName is required")
	}

	if etcdclient.IsV2(etcdVersion) && dataDir == "" {
		return nil, fmt.Errorf("DataDir is required for etcd v2")
	}

	m := &BackupController{
		clusterName:    clusterName,
		backupStore:    backupStore,
		dataDir:        dataDir,
		clientUrls:     clientUrls,
		etcdVersion:    etcdVersion,
		backupInterval: 5 * time.Minute,
		backupCleanup:  NewBackupCleanup(backupStore),
	}
	return m, nil
}

func (m *BackupController) Run(ctx context.Context) {
	contextutil.Forever(ctx,
		loopInterval, // We do our own sleeping
		func() {
			err := m.run(ctx)
			if err != nil {
				glog.Warningf("unexpected error running backup controller loop: %v", err)
			}
		})
}

func (m *BackupController) run(ctx context.Context) error {
	glog.V(2).Infof("starting backup controller iteration")

	etcdClient, err := etcdclient.NewClient(m.etcdVersion, m.clientUrls)
	if err != nil {
		return fmt.Errorf("unable to reach etcd on %s: %v", m.clientUrls, err)
	}
	members, err := etcdClient.ListMembers(ctx)
	if err != nil {
		etcdClient.Close()
		return fmt.Errorf("unable to list members on %s: %v", m.clientUrls, err)
	}

	self, err := etcdClient.LocalNodeInfo(ctx)
	etcdClient.Close()
	if err != nil {
		return fmt.Errorf("unable to get node state on %s: %v", m.clientUrls, err)
	}

	if !self.IsLeader {
		glog.V(2).Infof("Not leader, won't backup")
		return nil
	}

	return m.maybeBackup(ctx, members)
}

func (m *BackupController) maybeBackup(ctx context.Context, members []*etcdclient.EtcdProcessMember) error {
	now := time.Now()

	shouldBackup := now.Sub(m.lastBackup) > m.backupInterval
	if !shouldBackup {
		return nil
	}

	backup, err := m.doClusterBackup(ctx, members)
	if err != nil {
		return err
	}

	glog.Infof("took backup: %v", backup)
	m.lastBackup = now

	if err := m.backupCleanup.MaybeDoBackupMaintenance(ctx); err != nil {
		glog.Warningf("error during backup cleanup: %v", err)
	}

	return nil
}

func (m *BackupController) doClusterBackup(ctx context.Context, members []*etcdclient.EtcdProcessMember) (*protoetcd.DoBackupResponse, error) {
	info := &protoetcd.BackupInfo{
		ClusterSpec: &protoetcd.ClusterSpec{
			MemberCount: int32(len(members)),
			EtcdVersion: m.etcdVersion,
		},
		EtcdVersion: m.etcdVersion,
	}

	return etcd.DoBackup(m.backupStore, info, m.dataDir, m.clientUrls)
}
