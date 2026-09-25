//go:build e2e

// Package e2e runs the operator on a kind cluster with the fake server image.
//
//	mise run e2e
//
// Environment:
//
//	E2E_CLUSTER     kind cluster name (default mc-e2e)
//	E2E_KUBECONFIG  reuse an existing cluster (images must already be loaded);
//	                no cluster is created or destroyed
//	E2E_KEEP=1      keep the kind cluster after the run
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/support/kind"

	"github.com/andreabedini/minecraft-operator/api/v1alpha1"
)

const (
	operatorNamespace = "minecraft-operator"
	operatorName      = "minecraft-operator"
)

var (
	images = []string{
		"docker.io/minecraft-operator/operator:e2e",
		"docker.io/minecraft-operator/supervisor:e2e",
		"docker.io/minecraft-operator/fakeserver:e2e",
	}
	testenv env.Environment
	scheme  = runtime.NewScheme()
	// testNamespace holds the instances; one per run.
	testNamespace string
)

func TestMain(m *testing.M) {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	clusterName := envOr("E2E_CLUSTER", "mc-e2e")
	testNamespace = envconf.RandomName("mc", 10)
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		panic(err)
	}

	if kubeconfig := os.Getenv("E2E_KUBECONFIG"); kubeconfig != "" {
		testenv = env.NewWithKubeConfig(kubeconfig)
	} else {
		testenv = env.New()
		testenv.Setup(envfuncs.CreateClusterWithConfig(kind.NewProvider(), clusterName, filepath.Join(repoRoot, "hack", "kind-config.yaml")))
		for _, img := range images {
			testenv.Setup(envfuncs.LoadImageToCluster(clusterName, img))
		}
		if os.Getenv("E2E_KEEP") == "" {
			testenv.Finish(envfuncs.DestroyCluster(clusterName))
		}
	}
	testenv.Setup(
		applyKustomize(filepath.Join(repoRoot, "config", "e2e")),
		waitForDeployment(operatorNamespace, operatorName, 3*time.Minute),
		waitForDeployment(operatorNamespace, "fake-upstream", 2*time.Minute),
		envfuncs.CreateNamespace(testNamespace),
	)
	testenv.Finish(envfuncs.DeleteNamespace(testNamespace))
	os.Exit(testenv.Run(m))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// applyKustomize runs kubectl apply -k against the environment's cluster.
func applyKustomize(dir string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", cfg.KubeconfigFile(), "apply", "-k", dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return ctx, fmt.Errorf("kubectl apply -k %s: %w\n%s", dir, err, out)
		}
		return ctx, nil
	}
}

// waitForDeployment blocks until the deployment has its replicas available.
func waitForDeployment(namespace, name string, timeout time.Duration) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		c, err := newClient(cfg)
		if err != nil {
			return ctx, err
		}
		deadline := time.Now().Add(timeout)
		for {
			var dep appsv1.Deployment
			err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &dep)
			if err == nil && dep.Status.AvailableReplicas >= 1 {
				return ctx, nil
			}
			if time.Now().After(deadline) {
				return ctx, fmt.Errorf("deployment %s/%s not available after %s (last error %v)", namespace, name, timeout, err)
			}
			time.Sleep(2 * time.Second)
		}
	}
}

// newClient returns a controller-runtime client with the operator's scheme.
func newClient(cfg *envconf.Config) (client.Client, error) {
	return client.New(cfg.Client().RESTConfig(), client.Options{Scheme: scheme})
}
