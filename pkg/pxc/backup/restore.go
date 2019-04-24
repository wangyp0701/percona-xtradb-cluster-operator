package backup

import (
	"context"
	"time"

	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/percona/percona-xtradb-cluster-operator/pkg/apis/pxc/v1alpha1"
)

func Restore(bcp *api.PerconaXtraDBBackup, cl client.Client) error {
	if len(bcp.Status.Destination) > 6 {

		switch {
		case bcp.Status.Destination[:4] == "pvc/":
			return errors.Wrap(restorePVC(bcp, cl, bcp.Status.Destination[4:]), "pvc")
		case bcp.Status.Destination[:5] == "s3://":
			return errors.Wrap(restoreS3(bcp, cl, bcp.Status.Destination[5:]), "s3")
		}
	}

	return errors.Errorf("unknown destination %s", bcp.Status.Destination)
}

func restorePVC(bcp *api.PerconaXtraDBBackup, cl client.Client, pvcName string) error {
	svc := corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-src-" + bcp.Spec.PXCCluster,
			Namespace: bcp.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"name": "restore-src-" + bcp.Spec.PXCCluster,
			},
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Port: 3307,
					Name: "ncat",
				},
			},
		},
	}

	podPVC := corev1.Volume{
		Name: "backup",
	}
	podPVC.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: pvcName,
	}
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-src-" + bcp.Spec.PXCCluster,
			Namespace: bcp.Namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "ncat",
					Image:           "percona/percona-xtradb-cluster-operator:0.3.0-backup",
					ImagePullPolicy: corev1.PullAlways,
					Command: []string{
						"bash",
						"-exc",
						"cat /backup/xtrabackup.stream | ncat -l --send-only 3307",
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "backup",
							MountPath: "/backup",
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyAlways,
			Volumes: []corev1.Volume{
				podPVC,
			},
		},
	}

	jobPVC := corev1.Volume{
		Name: "datadir",
	}
	jobPVC.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: "datadir-" + bcp.Spec.PXCCluster + "-pxc-0",
	}
	job := batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-job-" + bcp.Spec.PXCCluster,
			Namespace: bcp.Namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "xtrabackup",
							Image:           "percona/percona-xtradb-cluster-operator:0.3.0-backup",
							ImagePullPolicy: corev1.PullAlways,
							Command: []string{
								"bash",
								"-exc",
								`|
								ping -c1 restore-src-$cluster || :
								rm -rf /datadir/*
								ncat restore-src-$cluster 3307 | xbstream -x -C /datadir
								xtrabackup --prepare --target-dir=/datadir`,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "backup",
									MountPath: "/backup",
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
					Volumes: []corev1.Volume{
						jobPVC,
					},
				},
			},
			BackoffLimit: func(i int32) *int32 { return &i }(4),
		},
	}

	cl.Delete(context.TODO(), &job)
	cl.Delete(context.TODO(), &svc)
	cl.Delete(context.TODO(), &pod)

	err := cl.Create(context.TODO(), &svc)
	if err != nil {
		errors.Wrap(err, "create service")
	}
	err = cl.Create(context.TODO(), &pod)
	if err != nil {
		errors.Wrap(err, "create pod")
	}
	err = cl.Create(context.TODO(), &job)
	if err != nil {
		errors.Wrap(err, "create job")
	}

	defer func() {
		cl.Delete(context.TODO(), &svc)
		cl.Delete(context.TODO(), &pod)
	}()

	for {
		checkJob := batchv1.Job{}
		err := cl.Get(context.TODO(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &checkJob)
		if err != nil {
			errors.Wrap(err, "get job status")
		}
		for _, cond := range checkJob.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
				return nil
			}
		}
		time.Sleep(time.Second * 1)
	}

	return nil
}

func restoreS3(bcp *api.PerconaXtraDBBackup, cl client.Client, s3 string) error {
	jobPVC := corev1.Volume{
		Name: "datadir",
	}
	jobPVC.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: "datadir-" + bcp.Spec.PXCCluster + "-pxc-0",
	}
	job := batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-job-" + bcp.Spec.PXCCluster,
			Namespace: bcp.Namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "xtrabackup",
							Image:           "percona/percona-xtradb-cluster-operator:0.3.0-backup",
							ImagePullPolicy: corev1.PullAlways,
							Command: []string{
								"bash",
								"-exc",
								`|
								mc -C /tmp/mc config host add dest "${AWS_ENDPOINT_URL:-https://s3.amazonaws.com}" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY"
								mc -C /tmp/mc ls dest/$backup_bucket/$backup_key
								rm -rf /datadir/*
								mc -C /tmp/mc cat dest/$backup_bucket/$backup_key | xbstream -x -C /datadir
								xtrabackup --prepare --target-dir=/datadir`,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "backup",
									MountPath: "/backup",
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
					Volumes: []corev1.Volume{
						jobPVC,
					},
				},
			},
			BackoffLimit: func(i int32) *int32 { return &i }(4),
		},
	}

	err := cl.Create(context.TODO(), &job)
	if err != nil {
		errors.Wrap(err, "create job")
	}

	for {
		checkJob := batchv1.Job{}
		err := cl.Get(context.TODO(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &checkJob)
		if err != nil {
			errors.Wrap(err, "get job status")
		}
		for _, cond := range checkJob.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
				return nil
			}
		}
		time.Sleep(time.Second * 1)
	}

	return nil
}
