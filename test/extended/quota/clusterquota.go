package quota

import (
	"context"
	"fmt"
	"time"

	g "github.com/onsi/ginkgo/v2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/kubernetes/test/e2e/framework"

	imagev1 "github.com/openshift/api/image/v1"
	quotav1 "github.com/openshift/api/quota/v1"
	quotaclient "github.com/openshift/client-go/quota/clientset/versioned"
	exutil "github.com/openshift/origin/test/extended/util"
)

var _ = g.Describe("[sig-api-machinery][Feature:ClusterResourceQuota]", func() {
	defer g.GinkgoRecover()
	oc := exutil.NewCLI("crq")

	g.Describe("Cluster resource quota", func() {
		g.It(fmt.Sprintf("should control resource limits across namespaces [apigroup:quota.openshift.io][apigroup:image.openshift.io]"), func() {
			t := g.GinkgoT(1)

			clusterAdminKubeClient := oc.AdminKubeClient()
			clusterAdminQuotaClient := oc.AdminQuotaClient()
			clusterAdminImageClient := oc.AdminImageClient()

			labelSelectorKey := "foo-" + oc.Namespace()
			cq := &quotav1.ClusterResourceQuota{
				ObjectMeta: metav1.ObjectMeta{Name: "overall-" + oc.Namespace()},
				Spec: quotav1.ClusterResourceQuotaSpec{
					Selector: quotav1.ClusterResourceQuotaSelector{
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelSelectorKey: "bar"}},
					},
					Quota: corev1.ResourceQuotaSpec{
						Hard: corev1.ResourceList{
							corev1.ResourceConfigMaps:   resource.MustParse("2"),
							"openshift.io/imagestreams": resource.MustParse("1"),
						},
					},
				},
			}

			const kubeRootCAName = "kube-root-ca.crt"
			framework.Logf("expecting ConfigMap %q to be present", kubeRootCAName)

			const serviceCAName = "openshift-service-ca.crt"
			framework.Logf("expecting ConfigMap %q to be present", serviceCAName)

			// Each namespace is expected to have a configmap each for kube root ca and service ca
			namespaceInitialCMCount := 2

			// Ensure quota includes the 2 mandatory configmaps
			// TODO(marun) Figure out why the added quantity isn't 2
			mandatoryCMQuantity := resource.NewQuantity(int64(namespaceInitialCMCount)*2, resource.DecimalSI)
			q := cq.Spec.Quota.Hard[corev1.ResourceConfigMaps]
			q.Add(*mandatoryCMQuantity)
			cq.Spec.Quota.Hard[corev1.ResourceConfigMaps] = q

			if _, err := clusterAdminQuotaClient.QuotaV1().ClusterResourceQuotas().Create(context.Background(), cq, metav1.CreateOptions{}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			oc.AddResourceToDelete(quotav1.GroupVersion.WithResource("clusterresourcequotas"), cq)

			firstProjectName := oc.SetupProject()
			secondProjectName := oc.SetupProject()

			// Wait for the creation of the mandatory configmaps before performing checks of quota
			// enforcement to ensure reliable test execution.
			for _, ns := range []string{firstProjectName, secondProjectName} {
				for _, cm := range []string{kubeRootCAName, serviceCAName} {
					_, err := exutil.WaitForCMState(context.Background(), clusterAdminKubeClient.CoreV1(), ns, cm, func(cm *corev1.ConfigMap) (bool, error) {
						// Any event means the CM is present
						framework.Logf("configmap %q is present in namespace %q", cm, ns)
						return true, nil
					})
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
				}
			}

			if err := labelNamespace(clusterAdminKubeClient.CoreV1(), labelSelectorKey, firstProjectName); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := labelNamespace(clusterAdminKubeClient.CoreV1(), labelSelectorKey, secondProjectName); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := waitForQuotaLabeling(clusterAdminQuotaClient, firstProjectName); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := waitForQuotaLabeling(clusterAdminQuotaClient, secondProjectName); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := waitForQuotaStatus(clusterAdminQuotaClient, cq.Name, func(quota *quotav1.ClusterResourceQuota) error {
				if !equality.Semantic.DeepEqual(quota.Spec.Quota.Hard, quota.Status.Total.Hard) {
					return fmt.Errorf("%#v != %#v", quota.Spec.Quota.Hard, quota.Status.Total.Hard)
				}
				return nil
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			configmap := &corev1.ConfigMap{}
			configmap.GenerateName = "test"
			if _, err := clusterAdminKubeClient.CoreV1().ConfigMaps(firstProjectName).Create(context.Background(), configmap, metav1.CreateOptions{}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := waitForQuotaStatus(clusterAdminQuotaClient, cq.Name, func(quota *quotav1.ClusterResourceQuota) error {
				expectedCount := int64(2*namespaceInitialCMCount + 1)
				q := quota.Status.Total.Used[corev1.ResourceConfigMaps]
				if i, ok := q.AsInt64(); ok {
					if i == expectedCount {
						return nil
					}
					return fmt.Errorf("%d != %d", i, expectedCount)
				}
				return fmt.Errorf("quota=%+v AsInt64() failed", q)
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, err := clusterAdminKubeClient.CoreV1().ConfigMaps(secondProjectName).Create(context.Background(), configmap, metav1.CreateOptions{}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := waitForQuotaStatus(clusterAdminQuotaClient, cq.Name, func(quota *quotav1.ClusterResourceQuota) error {
				expectedCount := int64(2*namespaceInitialCMCount + 2)
				q := quota.Status.Total.Used[corev1.ResourceConfigMaps]
				if i, ok := q.AsInt64(); ok {
					if i == expectedCount {
						return nil
					}
					return fmt.Errorf("%d != %d", i, expectedCount)
				}
				return fmt.Errorf("quota=%+v AsInt64() failed", q)
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, err := clusterAdminKubeClient.CoreV1().ConfigMaps(secondProjectName).Create(context.Background(), configmap, metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
				list, err := clusterAdminQuotaClient.QuotaV1().AppliedClusterResourceQuotas(secondProjectName).List(context.Background(), metav1.ListOptions{})
				if err == nil {
					t.Errorf("quota is %#v", list)
				}

				list2, err := clusterAdminKubeClient.CoreV1().ConfigMaps("").List(context.Background(), metav1.ListOptions{})
				if err == nil {
					t.Errorf("ConfigMaps is %#v", list2)
				}

				t.Fatalf("unexpected error: %v", err)
			}

			imagestream := &imagev1.ImageStream{}
			imagestream.GenerateName = "test"
			if _, err := clusterAdminImageClient.ImageV1().ImageStreams(firstProjectName).Create(context.Background(), imagestream, metav1.CreateOptions{}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := waitForQuotaStatus(clusterAdminQuotaClient, cq.Name, func(quota *quotav1.ClusterResourceQuota) error {
				q := quota.Status.Total.Used["openshift.io/imagestreams"]
				if i, ok := q.AsInt64(); ok {
					if i == 1 {
						return nil
					}
					return fmt.Errorf("%d != 1", i)
				}
				return fmt.Errorf("quota=%+v AsInt64() failed", q)
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if _, err := clusterAdminImageClient.ImageV1().ImageStreams(secondProjectName).Create(context.Background(), imagestream, metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
				list, err := clusterAdminQuotaClient.QuotaV1().AppliedClusterResourceQuotas(secondProjectName).List(context.Background(), metav1.ListOptions{})
				if err == nil {
					t.Errorf("quota is %#v", list)
				}

				list2, err := clusterAdminImageClient.ImageV1().ImageStreams("").List(context.Background(), metav1.ListOptions{})
				if err == nil {
					t.Errorf("ImageStreams is %#v", list2)
				}

				t.Fatalf("unexpected error: %v", err)
			}
		})
	})
})

func waitForQuotaLabeling(clusterAdminClient quotaclient.Interface, namespaceName string) error {
	return utilwait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (done bool, err error) {
		list, err := clusterAdminClient.QuotaV1().AppliedClusterResourceQuotas(namespaceName).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return false, nil
		}
		if len(list.Items) > 0 && len(list.Items[0].Status.Total.Hard) > 0 {
			return true, nil
		}
		return false, nil
	})
}

func labelNamespace(clusterAdminKubeClient corev1client.NamespacesGetter, labelKey, namespaceName string) error {
	ns1, err := clusterAdminKubeClient.Namespaces().Get(context.Background(), namespaceName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if ns1.Labels == nil {
		ns1.Labels = map[string]string{}
	}
	ns1.Labels[labelKey] = "bar"
	if _, err := clusterAdminKubeClient.Namespaces().Update(context.Background(), ns1, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

func waitForQuotaStatus(clusterAdminClient quotaclient.Interface, name string, conditionFn func(*quotav1.ClusterResourceQuota) error) error {
	var pollErr error
	err := utilwait.PollImmediate(100*time.Millisecond, 30*time.Second, func() (done bool, err error) {
		quota, err := clusterAdminClient.QuotaV1().ClusterResourceQuotas().Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			pollErr = err
			return false, nil
		}
		err = conditionFn(quota)
		if err == nil {
			return true, nil
		}
		pollErr = err
		return false, nil
	})
	if err == nil {
		// since now we run each process separately we need to wait for the informers
		// to catch up on the update and only then continue
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		err = fmt.Errorf("%s: %s", err, pollErr)
	}
	return err
}
