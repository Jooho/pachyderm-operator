package generators

import (
	"fmt"
	"strings"

	aimlv1beta1 "github.com/opdev/pachyderm-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func pachdEnvVarirables(pd *aimlv1beta1.Pachyderm) []corev1.EnvVar {
	envs := []corev1.EnvVar{
		{
			Name:  "POSTGRES_HOST",
			Value: pd.Spec.Pachd.Postgres.Host,
		},
		{
			Name:  "POSTGRES_PORT",
			Value: fmt.Sprintf("%d", pd.Spec.Pachd.Postgres.Port),
		},
		{
			Name:  "POSTGRES_SERVICE_SSL",
			Value: pd.Spec.Pachd.Postgres.SSL,
		},
		{ // enable loki logging
			Name:  "LOKI_LOGGING",
			Value: fmt.Sprintf("%t", pd.Spec.Pachd.LokiLogging),
		},
		{ // pachd root
			Name:  "PACH_ROOT",
			Value: "/pach",
		},
		{ // etcd prefix
			Name:  "ETCD_PREFIX",
			Value: "",
		},
		{ // storage backend
			Name:  "STORAGE_BACKEND",
			Value: strings.ToUpper(pd.Spec.Pachd.Storage.Backend),
		},
		{
			Name:  "STORAGE_PUT_FILE_CONCURRENCY_LIMIT",
			Value: fmt.Sprintf("%d", pd.Spec.Pachd.Storage.PutFileConcurrencyLimit),
		},
		{
			Name:  "STORAGE_UPLOAD_CONCURRENCY_LIMIT",
			Value: fmt.Sprintf("%d", pd.Spec.Pachd.Storage.PutFileConcurrencyLimit),
		},
	}

	if pd.Spec.Worker != nil {
		// TODO: auto set the WORKER_IMAGE and WORKER_SIDECAR_IMAGE
		//  environment variables automatically
		workerOptions := []corev1.EnvVar{
			{
				Name:  "WORKER_IMAGE",
				Value: getWorkerImage(pd),
			},
			{
				Name:  "WORKER_SIDECAR_IMAGE",
				Value: "pachyderm/pachd:2.0.0-alpha.25",
			},
			{
				Name:  "WORKER_IMAGE_PULL_POLICY",
				Value: pd.Spec.Worker.Image.PullPolicy,
			},
			{
				Name:  "WORKER_SERVICE_ACCOUNT",
				Value: pd.Spec.Worker.ServiceAccountName,
			},
		}
		envs = append(envs, workerOptions...)
	}

	if pd.Spec.Pachd.Image != nil {
		// image pull secret
		envs = append(envs, corev1.EnvVar{
			Name:  "IMAGE_PULL_SECRET",
			Value: pd.Spec.Pachd.Image.PullPolicy,
		})
	}

	// metrics
	if pd.Spec.Pachd.Metrics != nil {
		envs = append(envs, corev1.EnvVar{
			Name:  "METRICS",
			Value: fmt.Sprintf("%t", !pd.Spec.Pachd.Metrics.Disable),
		})

		if pd.Spec.Pachd.Metrics.Endpoint != "" {
			// TODO: check if this is still supported
			envs = append(envs, corev1.EnvVar{
				Name:  "METRICS_ENDPOINT",
				Value: pd.Spec.Pachd.Metrics.Endpoint,
			})
		}
	}

	// log level
	envs = append(envs, corev1.EnvVar{
		Name:  "LOG_LEVEL",
		Value: pd.Spec.Pachd.LogLevel,
	})

	// expose Docker socket
	envs = append(envs, corev1.EnvVar{
		Name:  "NO_EXPOSE_DOCKER_SOCKET",
		Value: fmt.Sprintf("%t", pd.Spec.Pachd.ExposeDockerSocket),
	})

	// TODO: check if this is still supported
	// block cache bytes
	// envs = append(envs, corev1.EnvVar{
	// 	Name:  "BLOCK_CACHE_BYTES",
	// 	Value: pd.Spec.Pachd.BlockCacheBytes,
	// })

	// TODO: check if this is still supported
	// disable pachyderm auth for testing
	// envs = append(envs, corev1.EnvVar{
	// 	Name:  "PACHYDERM_AUTHENTICATION_DISABLED_FOR_TESTING",
	// 	Value: fmt.Sprintf("%t", pd.Spec.Pachd.AuthenticationDisabledForTesting),
	// })

	// pachd namespace
	envs = append(envs, corev1.EnvVar{
		Name: "PACH_NAMESPACE",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				APIVersion: "v1",
				FieldPath:  "metadata.namespace",
			},
		},
	})

	// pachd memory request
	// TODO: handle error
	divisor, _ := resource.ParseQuantity("0")
	envs = append(envs, corev1.EnvVar{
		Name: "PACHD_MEMORY_REQUEST",
		ValueFrom: &corev1.EnvVarSource{
			ResourceFieldRef: &corev1.ResourceFieldSelector{
				ContainerName: "pachd",
				Divisor:       divisor,
				Resource:      "requests.memory",
			},
		},
	})

	// TODO: check if this is still supported
	// expose object API
	// envs = append(envs, corev1.EnvVar{
	// 	Name:  "EXPOSE_OBJECT_API",
	// 	Value: fmt.Sprintf("%t", pd.Spec.Pachd.ExposeObjectAPI),
	// })

	// require critical servers only
	envs = append(envs, corev1.EnvVar{
		Name:  "REQUIRE_CRITICAL_SERVERS_ONLY",
		Value: fmt.Sprintf("%t", pd.Spec.Pachd.RequireCriticalServers),
	})

	// pachd pod name
	envs = append(envs, corev1.EnvVar{
		Name: "PACHD_POD_NAME",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				APIVersion: "v1",
				FieldPath:  "metadata.name",
			},
		},
	})

	// Pachyderm Pipeline System(PPS)
	// worker GRPC port
	envs = append(envs, corev1.EnvVar{
		Name:  "PPS_WORKER_GRPC_PORT",
		Value: fmt.Sprintf("%d", pd.Spec.Pachd.PPSWorkerGRPCPort),
	})

	return envs
}

func getWorkerImage(pd *aimlv1beta1.Pachyderm) string {

	if pd.Spec.Worker != nil {
		if pd.Spec.Worker.Image != nil {
			workerImg := []string{
				pd.Spec.Worker.Image.Repository,
				pd.Spec.Worker.Image.ImageTag,
			}
			return strings.Join(workerImg, ":")
		}
	}

	// load default worker images
	return ""
}
