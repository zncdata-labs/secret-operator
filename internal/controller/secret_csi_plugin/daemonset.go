package secret_csi_plugin

import (
	"context"
	"errors"
	"time"

	secretsv1alpha1 "github.com/zncdata-labs/secret-operator/api/v1alpha1"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type DaemonSet struct {
	client client.Client
	cr     *secretsv1alpha1.SecretCSI

	secret         *secretsv1alpha1.SecretCSISpec
	serviceAccount string
}

func NewDaemonSet(client client.Client, cr *secretsv1alpha1.SecretCSI, secret *secretsv1alpha1.SecretCSISpec, serviceAccount string) *DaemonSet {
	return &DaemonSet{
		client:         client,
		cr:             cr,
		secret:         secret,
		serviceAccount: serviceAccount,
	}
}

func (r *DaemonSet) Reconcile(ctx context.Context) (ctrl.Result, error) {
	obj, err := r.makeDaemonset()
	if err != nil {
		return ctrl.Result{}, err
	}

	mutant, err := CreateOrUpdate(ctx, r.client, obj)
	if err != nil {
		return ctrl.Result{}, err
	} else if mutant {
		return ctrl.Result{RequeueAfter: time.Second * 10}, nil
	}
	return ctrl.Result{}, nil
}

func (r *DaemonSet) getName() string {
	return "csi-secrets"
}

func (r *DaemonSet) Satisfied(ctx context.Context) (bool, error) {
	obj := &appv1.DaemonSet{}
	err := r.client.Get(ctx, client.ObjectKey{
		Name:      r.getName(),
		Namespace: r.cr.GetNamespace(),
	}, obj)
	if err != nil {
		return false, err
	}

	if obj.Status.DesiredNumberScheduled == obj.Status.NumberReady {
		return true, nil
	}

	return false, errors.New("daemonset is not ready, number of ready pods is less than desired number of pods")
}

func (r *DaemonSet) getVolumes() []corev1.Volume {
	return []corev1.Volume{
		{
			Name: VOLUMES_MOUNTPOINT_DIR_NAME,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/lib/kubelet/pods",
					Type: func() *corev1.HostPathType {
						t := corev1.HostPathDirectoryOrCreate
						return &t
					}(),
				},
			},
		},
		{
			Name: VOLUMES_PLUGIN_DIR_NAME,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/lib/kubelet/plugins" + secretsv1alpha1.GroupVersion.Group,
					Type: func() *corev1.HostPathType {
						t := corev1.HostPathDirectoryOrCreate
						return &t
					}(),
				},
			},
		},
		{
			Name: VOLUMES_REGISTRATION_DIR_NAME,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/lib/kubelet/plugins_registry",
					Type: func() *corev1.HostPathType {
						t := corev1.HostPathDirectoryOrCreate
						return &t
					}(),
				},
			},
		},
	}
}

func (r *DaemonSet) makeDaemonset() (*appv1.DaemonSet, error) {

	obj := &appv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.getName(),
			Namespace: r.cr.GetNamespace(),
		},
		Spec: appv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": r.getName(),
				},
			},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: r.serviceAccount,
					Volumes:            r.getVolumes(),
					Containers: []corev1.Container{
						*r.makeCSIPluginContainer(r.secret.CSIPlugin),
						*r.makeNodeDriverRegistrar(r.secret.NodeDriverRegister),
						*r.makeProvisioner(r.secret.CSIProvisioner),
						*r.makeLivenessProbe(r.secret.LivenessProbe),
					},
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(r.cr, obj, r.client.Scheme()); err != nil {
		return nil, err
	}
	return obj, nil
}

func (r *DaemonSet) makeCSIPluginContainer(csi *secretsv1alpha1.CSIPluginSpec) *corev1.Container {
	privileged := true
	runAsUser := int64(0)
	obj := &corev1.Container{
		Name:            "csi-secrets",
		Image:           csi.Repository + ":" + csi.Tag,
		ImagePullPolicy: corev1.PullPolicy(csi.PullPolicy),
		SecurityContext: &corev1.SecurityContext{
			Privileged: &privileged,
			RunAsUser:  &runAsUser,
		},
		Env: []corev1.EnvVar{
			{
				Name: "NODE_NAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "spec.nodeName",
					},
				},
			},
			{
				Name:  "ADDRESS",
				Value: "unix:///csi/csi.sock",
			},
		},
		Args: []string{
			"-endpoint=$(ADDRESS)",
			"-nodeid=$(NODE_NAME)",
			"-zap-log-level=5",
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      VOLUMES_PLUGIN_DIR_NAME,
				MountPath: "/csi",
			},
			{
				Name:      VOLUMES_MOUNTPOINT_DIR_NAME,
				MountPath: "/var/lib/kubelet/pods",
			},
		},
	}

	return obj
}

func (r *DaemonSet) makeNodeDriverRegistrar(sidecar *secretsv1alpha1.NodeDriverRegisterSpec) *corev1.Container {
	obj := &corev1.Container{
		Name:            "node-driver-registrar",
		Image:           sidecar.Repository + ":" + sidecar.Tag,
		ImagePullPolicy: corev1.PullPolicy(sidecar.PullPolicy),
		Args: []string{
			"--v=5",
			"--csi-address=$(ADDRESS)",
			"--kubelet-registration-path=$(DRIVER_REG_SOCK_PATH)",
		},
		Env: []corev1.EnvVar{
			{
				Name:  "ADDRESS",
				Value: "unix:///csi/csi.sock",
			},
			{
				Name:  "DRIVER_REG_SOCK_PATH",
				Value: "/var/lib/kubelet/plugins" + secretsv1alpha1.GroupVersion.Group + "/csi.sock",
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      VOLUMES_REGISTRATION_DIR_NAME,
				MountPath: "/registration",
			},
			{
				Name:      VOLUMES_PLUGIN_DIR_NAME,
				MountPath: "/csi",
			},
		},
	}

	return obj
}

func (r *DaemonSet) makeProvisioner(sidecar *secretsv1alpha1.CSIProvisionerSpec) *corev1.Container {
	obj := &corev1.Container{
		Name:            "csi-provisioner",
		Image:           sidecar.Repository + ":" + sidecar.Tag,
		ImagePullPolicy: corev1.PullPolicy(sidecar.PullPolicy),
		Args: []string{
			"--v=5",
			"--csi-address=$(ADDRESS)",
			"--feature-gates=Topology=true",
			"--extra-create-metadata",
		},
		Env: []corev1.EnvVar{
			{
				Name:  "ADDRESS",
				Value: "unix:///csi/csi.sock",
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      VOLUMES_PLUGIN_DIR_NAME,
				MountPath: "/csi",
			},
		},
	}

	return obj
}

func (r *DaemonSet) makeLivenessProbe(sidecar *secretsv1alpha1.LivenessProbeSpec) *corev1.Container {
	obj := &corev1.Container{
		Name:            "liveness-probe",
		Image:           sidecar.Repository + ":" + sidecar.Tag,
		ImagePullPolicy: corev1.PullPolicy(sidecar.PullPolicy),
		Args: []string{
			"--csi-address=$(ADDRESS)",
			"--health-port=9808",
		},
		Env: []corev1.EnvVar{
			{
				Name:  "ADDRESS",
				Value: "unix:///csi/csi.sock",
			},
		},
		Ports: []corev1.ContainerPort{
			{
				ContainerPort: 9808,
				Name:          "healthz",
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      VOLUMES_PLUGIN_DIR_NAME,
				MountPath: "/csi",
			},
		},
	}

	return obj
}
