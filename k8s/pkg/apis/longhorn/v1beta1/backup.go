package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type BackupState string

const (
	BackupStateInProgress = BackupState("InProgress")
	BackupStateCompleted  = BackupState("Completed")
	BackupStateError      = BackupState("Error")
	BackupStateUnknown    = BackupState("Unknown")
)

type SnapshotBackupSpec struct {
	SyncRequestedAt metav1.Time       `json:"syncRequestedAt"`
	SnapshotName    string            `json:"snapshotName"`
	Labels          map[string]string `json:"labels"`
}

type SnapshotBackupStatus struct {
	OwnerID                string            `json:"ownerID"`
	State                  BackupState       `json:"state"`
	URL                    string            `json:"url"`
	SnapshotName           string            `json:"snapshotName"`
	SnapshotCreatedAt      string            `json:"snapshotCreatedAt"`
	BackupCreatedAt        string            `json:"backupCreatedAt"`
	Size                   string            `json:"size"`
	Labels                 map[string]string `json:"labels"`
	Messages               map[string]string `json:"messages"`
	VolumeName             string            `json:"volumeName"`
	VolumeSize             string            `json:"volumeSize"`
	VolumeCreated          string            `json:"volumeCreated"`
	VolumeBackingImageName string            `json:"volumeBackingImageName"`
	LastSyncedAt           metav1.Time       `json:"lastSyncedAt"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type Backup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              SnapshotBackupSpec   `json:"spec"`
	Status            SnapshotBackupStatus `json:"status"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type BackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []Backup `json:"items"`
}