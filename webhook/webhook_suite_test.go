// Copyright 2019 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package webhook

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	mgr "sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/envtest/printer"

	"github.com/stolostron/multicloud-operators-application/pkg/apis"
	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
)

const (
	StartTimeout = 60 // seconds
)

var testEnv *envtest.Environment
var k8sManager mgr.Manager
var k8sClient client.Client
var cfg *rest.Config

var (
	webhookValidatorName = "test-suite-webhook"
	stop                 = ctrl.SetupSignalHandler()
)

func TestMain(m *testing.M) {
	t := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "deploy", "crds"),
			filepath.Join("..", "..", "..", "hack", "test"),
		},
	}

	apis.AddToScheme(scheme.Scheme)

	var err error
	if cfg, err = t.Start(); err != nil {
		klog.Fatal(err)
	}

	if k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme}); err != nil {
		klog.Fatal(err)
	}

	err = k8sClient.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
	})
	if err != nil {
		klog.Fatal(err)
	}

	code := m.Run()

	t.Stop()
	os.Exit(code)
}

func TestApplicationWebhook(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecsWithDefaultAndCustomReporters(t,
		"Application webhook",
		[]Reporter{printer.NewlineReporter{}})
}

var _ = BeforeSuite(func(done Done) {
	By("bootstrapping test environment")

	t := true
	if os.Getenv("TEST_USE_EXISTING_CLUSTER") == "true" {
		testEnv = &envtest.Environment{
			UseExistingCluster: &t,
		}
	} else {
		customAPIServerFlags := []string{"--disable-admission-plugins=NamespaceLifecycle,LimitRanger,ServiceAccount," +
			"TaintNodesByCondition,Priority,DefaultTolerationSeconds,DefaultStorageClass,StorageObjectInUseProtection," +
			"PersistentVolumeClaimResize,ResourceQuota",
		}

		//nolint
		apiServerFlags := append([]string(nil), envtest.DefaultKubeAPIServerFlags...)
		apiServerFlags = append(apiServerFlags, customAPIServerFlags...)

		testEnv = &envtest.Environment{
			CRDDirectoryPaths:  []string{filepath.Join("..", "..", "deploy", "crds")},
			KubeAPIServerFlags: apiServerFlags,
		}
	}

	var err error
	// be careful, if we use shorthand assignment, the the cCfg will be a local variable
	initializeWebhookInEnvironment()
	cfg, err := testEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(cfg).ToNot(BeNil())

	err = apis.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	k8sManager, err = mgr.New(cfg, mgr.Options{
		MetricsBindAddress: "0",
		Port:               testEnv.WebhookInstallOptions.LocalServingPort,
		Host:               testEnv.WebhookInstallOptions.LocalServingHost,
		CertDir:            testEnv.WebhookInstallOptions.LocalServingCertDir,
	})

	Expect(err).NotTo(HaveOccurred())

	hookServer := k8sManager.GetWebhookServer()

	k8sClient, err = client.New(testEnv.Config, client.Options{})
	Expect(err).NotTo(HaveOccurred())

	testNs := "default"
	os.Setenv("POD_NAMESPACE", testNs)
	os.Setenv("DEPLOYMENT_LABEL", testNs)

	certDir := filepath.Join(os.TempDir(), "k8s-webhook-server", "application-serving-certs")

	_, err = WireUpWebhook(k8sClient, k8sManager, hookServer, certDir)

	Expect(err).ToNot(HaveOccurred())

	go func() {
		Expect(hookServer.Start(stop)).Should(Succeed())
	}()

	close(done)
}, StartTimeout)

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	gexec.KillAndWait(5 * time.Second)
	Expect(testEnv.Stop()).ToNot(HaveOccurred())
})

func initializeWebhookInEnvironment() {
	namespacedScopeV1 := admissionv1.NamespacedScope
	failedTypeV1 := admissionv1.Fail
	equivalentTypeV1 := admissionv1.Equivalent
	noSideEffectsV1 := admissionv1.SideEffectClassNone
	webhookPathV1 := ValidatorPath

	vwc := []*admissionv1.ValidatingWebhookConfiguration{}
	vwc = append(vwc, &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: webhookValidatorName,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "ValidatingWebhookConfiguration",
			APIVersion: "admissionregistration.k8s.io/v1beta1",
		},
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				Name:                    webhookName,
				AdmissionReviewVersions: []string{"v1beta1"},
				Rules: []admissionv1.RuleWithOperations{
					{
						Operations: []admissionv1.OperationType{"CREATE", "UPDATE"},
						Rule: admissionv1.Rule{
							APIGroups:   []string{appv1beta1.GroupVersion.Group},
							APIVersions: []string{appv1beta1.GroupVersion.Version},
							Resources:   []string{resourceName},
							Scope:       &namespacedScopeV1,
						},
					},
				},
				FailurePolicy: &failedTypeV1,
				MatchPolicy:   &equivalentTypeV1,
				SideEffects:   &noSideEffectsV1,
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Name:      "application-validation-service",
						Namespace: "default",
						Path:      &webhookPathV1,
					},
				},
			},
		},
	})

	testEnv.WebhookInstallOptions = envtest.WebhookInstallOptions{
		ValidatingWebhooks: vwc,
	}
}

// StartTestManager adds recFn
func StartTestManager(ctx context.Context, mgr mgr.Manager, g *GomegaWithT) *sync.WaitGroup {
	wg := &sync.WaitGroup{}
	wg.Add(1)

	go func() {
		defer wg.Done()
		mgr.Start(ctx)
	}()

	return wg
}
