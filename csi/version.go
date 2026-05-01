package csi

const (
	DriverName    = "local-path.appmana.io"
	DriverVersion = "0.1.0"

	TopologyKeyNode = "kubernetes.io/hostname"

	// Volume context keys returned to csi-provisioner and propagated to the PV.
	VolumeContextNode      = "node"
	VolumeContextPath      = "path"
	VolumeContextFSType    = "fsType"
	VolumeContextQuotaType = "quotaType"

	// Standard csi-provisioner-injected parameters when --extra-create-metadata=true.
	ParamPVCName      = "csi.storage.k8s.io/pvc/name"
	ParamPVCNamespace = "csi.storage.k8s.io/pvc/namespace"
	ParamPVName       = "csi.storage.k8s.io/pv/name"
)
