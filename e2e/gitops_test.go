package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	kluctlv1 "github.com/kluctl/kluctl/v2/api/v1beta1"
	test_utils "github.com/kluctl/kluctl/v2/e2e/test-utils"
	"github.com/kluctl/kluctl/v2/e2e/test_project"
	types2 "github.com/kluctl/kluctl/v2/pkg/types"
	"github.com/kluctl/kluctl/v2/pkg/utils/uo"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/suite"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"math/rand"
	"os"
	"path/filepath"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

const (
	timeout  = time.Second * 300
	interval = time.Second * 5
)

func init() {
	// this must be called in the first 30 seconds of startup, so we have to do it here at init() time
	ctrl.SetLogger(klog.NewKlogr())
}

type GitopsTestSuite struct {
	suite.Suite

	k *test_utils.EnvTestCluster

	cancelController context.CancelFunc

	deployments []client.ObjectKey
}

func (suite *GitopsTestSuite) SetupSuite() {
	suite.startCluster()
	suite.startController()
}

func (suite *GitopsTestSuite) TearDownSuite() {
	if suite.cancelController != nil {
		suite.cancelController()
	}

	if suite.k != nil {
		suite.k.Stop()
	}
}

func (suite *GitopsTestSuite) TearDownTest() {
	g := NewWithT(suite.T())

	for _, key := range suite.deployments {
		suite.deleteKluctlDeployment(key)
	}

	g.Eventually(func() bool {
		for _, key := range suite.deployments {
			var kd kluctlv1.KluctlDeployment
			err := suite.k.Client.Get(context.TODO(), key, &kd)
			if err == nil {
				return false
			}
		}
		return true
	}, timeout, time.Second).Should(BeTrue())

	suite.deployments = nil
}

func (suite *GitopsTestSuite) startCluster() {
	suite.k = test_utils.CreateEnvTestCluster("context1")
	suite.k.CRDDirectoryPaths = []string{"../config/crd/bases"}

	err := suite.k.Start()
	if err != nil {
		suite.T().Fatal(err)
	}
}

func (suite *GitopsTestSuite) startController() {
	tmpKubeconfig := filepath.Join(suite.T().TempDir(), "kubeconfig")
	err := os.WriteFile(tmpKubeconfig, suite.k.Kubeconfig, 0o600)
	if err != nil {
		suite.T().Fatal(err)
	}

	ctx, ctxCancel := context.WithCancel(context.Background())
	args := []string{
		"controller",
		"run",
		"--kubeconfig",
		tmpKubeconfig,
		"--context",
		"context1",
	}
	done := make(chan struct{})
	go func() {
		_, _, err := test_project.KluctlExecute(suite.T(), ctx, args...)
		if err != nil {
			suite.T().Error(err)
		}
		close(done)
	}()

	cancel := func() {
		ctxCancel()
		<-done
	}
	suite.cancelController = cancel
}

func (suite *GitopsTestSuite) triggerReconcile(key client.ObjectKey) string {
	reconcileId := fmt.Sprintf("%d", rand.Int63())

	suite.updateKluctlDeployment(key, func(kd *kluctlv1.KluctlDeployment) {
		a := kd.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a[kluctlv1.KluctlRequestReconcileAnnotation] = reconcileId
		kd.SetAnnotations(a)
	})
	return reconcileId
}

func (suite *GitopsTestSuite) waitForReconcile(key client.ObjectKey) {
	g := gomega.NewWithT(suite.T())

	reconcileId := suite.triggerReconcile(key)

	g.Eventually(func() bool {
		var kd kluctlv1.KluctlDeployment
		err := suite.k.Client.Get(context.TODO(), key, &kd)
		g.Expect(err).To(Succeed())
		return kd.Status.LastHandledReconcileAt == reconcileId
	}, timeout, time.Second).Should(BeTrue())
}

func (suite *GitopsTestSuite) waitForCommit(key client.ObjectKey, commit string) {
	g := gomega.NewWithT(suite.T())

	reconcileId := suite.triggerReconcile(key)

	g.Eventually(func() bool {
		var kd kluctlv1.KluctlDeployment
		_ = suite.k.Client.Get(context.Background(), key, &kd)
		return kd.Status.LastHandledReconcileAt == reconcileId && kd.Status.ObservedCommit == commit
	}, timeout, time.Second).Should(BeTrue())
}

func (suite *GitopsTestSuite) createKluctlDeployment(p *test_project.TestProject, target string, args map[string]any) client.ObjectKey {
	return suite.createKluctlDeployment2(p, target, args, func(kd *kluctlv1.KluctlDeployment) {
		kd.Spec.Source.Git = &kluctlv1.ProjectSourceGit{
			URL: *types2.ParseGitUrlMust(p.GitUrl()),
		}
	})
}

func (suite *GitopsTestSuite) createKluctlDeployment2(p *test_project.TestProject, target string, args map[string]any, modify ...func(kd *kluctlv1.KluctlDeployment)) client.ObjectKey {
	gitopsNs := p.TestSlug() + "-gitops"
	createNamespace(suite.T(), suite.k, gitopsNs)

	jargs, err := json.Marshal(args)
	if err != nil {
		suite.T().Fatal(err)
	}

	kluctlDeployment := &kluctlv1.KluctlDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.TestSlug(),
			Namespace: gitopsNs,
		},
		Spec: kluctlv1.KluctlDeploymentSpec{
			Interval: metav1.Duration{Duration: interval},
			Timeout:  &metav1.Duration{Duration: timeout},
			Target:   &target,
			Args: &runtime.RawExtension{
				Raw: jargs,
			},
		},
	}

	for _, m := range modify {
		if m != nil {
			m(kluctlDeployment)
		}
	}

	err = suite.k.Client.Create(context.Background(), kluctlDeployment)
	if err != nil {
		suite.T().Fatal(err)
	}

	key := client.ObjectKeyFromObject(kluctlDeployment)
	suite.deployments = append(suite.deployments, key)
	return key
}

func (suite *GitopsTestSuite) updateKluctlDeployment(key client.ObjectKey, update func(kd *kluctlv1.KluctlDeployment)) *kluctlv1.KluctlDeployment {
	g := NewWithT(suite.T())

	var kd kluctlv1.KluctlDeployment
	err := suite.k.Client.Get(context.TODO(), key, &kd)
	g.Expect(err).To(Succeed())

	patch := client.MergeFrom(kd.DeepCopy())

	update(&kd)

	err = suite.k.Client.Patch(context.TODO(), &kd, patch, client.FieldOwner("kubectl"))
	g.Expect(err).To(Succeed())

	return &kd
}

func (suite *GitopsTestSuite) deleteKluctlDeployment(key client.ObjectKey) {
	g := NewWithT(suite.T())

	var kd kluctlv1.KluctlDeployment
	kd.Name = key.Name
	kd.Namespace = key.Namespace
	err := suite.k.Client.Delete(context.Background(), &kd)
	if err != nil && !errors.IsNotFound(err) {
		g.Expect(err).To(Succeed())
	}
}

func (suite *GitopsTestSuite) TestGitOpsFieldManager() {
	g := NewWithT(suite.T())

	p := test_project.NewTestProject(suite.T())
	createNamespace(suite.T(), suite.k, p.TestSlug())

	p.UpdateTarget("target1", nil)
	p.AddKustomizeDeployment("d1", []test_project.KustomizeResource{
		{Name: "cm1.yaml", Content: uo.FromStringMust(`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
  namespace: "{{ args.namespace }}"
data:
  k1: v1
  k2: "{{ args.k2 + 1 }}"
`)},
	}, nil)

	key := suite.createKluctlDeployment(p, "target1", map[string]any{
		"namespace": p.TestSlug(),
		"k2":        42,
	})

	suite.Run("initial deployment", func() {
		suite.waitForCommit(key, getHeadRevision(suite.T(), p))
	})

	suite.updateKluctlDeployment(key, func(kd *kluctlv1.KluctlDeployment) {
		kd.Spec.DeployInterval = &kluctlv1.SafeDuration{Duration: metav1.Duration{Duration: interval}}
	})

	cm := &corev1.ConfigMap{}

	suite.Run("cm1 is deployed", func() {
		err := suite.k.Client.Get(context.TODO(), client.ObjectKey{
			Name:      "cm1",
			Namespace: p.TestSlug(),
		}, cm)
		g.Expect(err).To(Succeed())
		g.Expect(cm.Data).To(HaveKeyWithValue("k1", "v1"))
		g.Expect(cm.Data).To(HaveKeyWithValue("k2", "43"))
	})

	suite.Run("cm1 is modified and restored", func() {
		cm.Data["k1"] = "v2"
		err := suite.k.Client.Update(context.TODO(), cm, client.FieldOwner("kubectl"))
		g.Expect(err).To(Succeed())

		g.Eventually(func() bool {
			err := suite.k.Client.Get(context.TODO(), client.ObjectKey{
				Name:      "cm1",
				Namespace: p.TestSlug(),
			}, cm)
			g.Expect(err).To(Succeed())
			return cm.Data["k1"] == "v1"
		}, timeout, time.Second).Should(BeTrue())
	})

	suite.Run("cm1 gets a key added which is not modified by the controller", func() {
		cm.Data["k1"] = "v2"
		cm.Data["k3"] = "v3"
		err := suite.k.Client.Update(context.TODO(), cm, client.FieldOwner("kubectl"))
		g.Expect(err).To(Succeed())

		g.Eventually(func() bool {
			err := suite.k.Client.Get(context.TODO(), client.ObjectKey{
				Name:      "cm1",
				Namespace: p.TestSlug(),
			}, cm)
			g.Expect(err).To(Succeed())
			return cm.Data["k1"] == "v1"
		}, timeout, time.Second).Should(BeTrue())

		g.Expect(cm.Data).To(HaveKeyWithValue("k3", "v3"))
	})

	suite.Run("cm1 gets modified with another field manager", func() {
		patch := client.MergeFrom(cm.DeepCopy())
		cm.Data["k1"] = "v2"

		err := suite.k.Client.Patch(context.TODO(), cm, patch, client.FieldOwner("test-field-manager"))
		g.Expect(err).To(Succeed())

		for i := 0; i < 2; i++ {
			suite.waitForReconcile(key)
		}

		err = suite.k.Client.Get(context.TODO(), client.ObjectKey{
			Name:      "cm1",
			Namespace: p.TestSlug(),
		}, cm)
		g.Expect(err).To(Succeed())
		g.Expect(cm.Data).To(HaveKeyWithValue("k1", "v2"))
	})

	suite.updateKluctlDeployment(key, func(kd *kluctlv1.KluctlDeployment) {
		kd.Spec.ForceApply = true
	})

	suite.Run("forceApply is true and cm1 gets restored even with another field manager", func() {
		patch := client.MergeFrom(cm.DeepCopy())
		cm.Data["k1"] = "v2"

		err := suite.k.Client.Patch(context.TODO(), cm, patch, client.FieldOwner("test-field-manager"))
		g.Expect(err).To(Succeed())

		g.Eventually(func() bool {
			err := suite.k.Client.Get(context.TODO(), client.ObjectKey{
				Name:      "cm1",
				Namespace: p.TestSlug(),
			}, cm)
			g.Expect(err).To(Succeed())
			return cm.Data["k1"] == "v1"
		}, timeout, time.Second).Should(BeTrue())
	})
}

func (suite *GitopsTestSuite) TestKluctlDeploymentReconciler_Prune() {
	g := NewWithT(suite.T())

	p := test_project.NewTestProject(suite.T())
	createNamespace(suite.T(), suite.k, p.TestSlug())

	p.UpdateTarget("target1", nil)

	p.AddKustomizeDeployment("d1", []test_project.KustomizeResource{
		{Name: "cm1.yaml", Content: uo.FromStringMust(`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
  namespace: "{{ args.namespace }}"
data:
  k1: v1
`)},
	}, nil)
	p.AddKustomizeDeployment("d2", []test_project.KustomizeResource{
		{Name: "cm2.yaml", Content: uo.FromStringMust(`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm2
  namespace: "{{ args.namespace }}"
data:
  k1: v1
`)},
	}, nil)

	key := suite.createKluctlDeployment(p, "target1", map[string]any{
		"namespace": p.TestSlug(),
	})

	suite.waitForCommit(key, getHeadRevision(suite.T(), p))

	cm := &corev1.ConfigMap{}

	suite.Run("cm1 and cm2 got deployed", func() {
		err := suite.k.Client.Get(context.TODO(), client.ObjectKey{
			Name:      "cm1",
			Namespace: p.TestSlug(),
		}, cm)
		g.Expect(err).To(Succeed())
		err = suite.k.Client.Get(context.TODO(), client.ObjectKey{
			Name:      "cm2",
			Namespace: p.TestSlug(),
		}, cm)
		g.Expect(err).To(Succeed())
	})

	p.UpdateDeploymentYaml("", func(o *uo.UnstructuredObject) error {
		_ = o.RemoveNestedField("deployments", 1)
		return nil
	})

	g.Eventually(func() bool {
		var obj kluctlv1.KluctlDeployment
		_ = suite.k.Client.Get(context.Background(), key, &obj)
		if obj.Status.LastDeployResult == nil {
			return false
		}
		ldr, err := obj.Status.GetLastDeployResult()
		g.Expect(err).To(Succeed())
		return ldr.GitInfo.Commit == getHeadRevision(suite.T(), p)
	}, timeout, time.Second).Should(BeTrue())

	suite.Run("cm1 and cm2 were not deleted", func() {
		err := suite.k.Client.Get(context.TODO(), client.ObjectKey{
			Name:      "cm1",
			Namespace: p.TestSlug(),
		}, cm)
		g.Expect(err).To(Succeed())
		err = suite.k.Client.Get(context.TODO(), client.ObjectKey{
			Name:      "cm2",
			Namespace: p.TestSlug(),
		}, cm)
		g.Expect(err).To(Succeed())
	})

	suite.updateKluctlDeployment(key, func(kd *kluctlv1.KluctlDeployment) {
		kd.Spec.Prune = true
	})

	suite.waitForReconcile(key)

	suite.Run("cm1 did not get deleted and cm2 got deleted", func() {
		err := suite.k.Client.Get(context.TODO(), client.ObjectKey{
			Name:      "cm1",
			Namespace: p.TestSlug(),
		}, cm)
		g.Expect(err).To(Succeed())
		err = suite.k.Client.Get(context.TODO(), client.ObjectKey{
			Name:      "cm2",
			Namespace: p.TestSlug(),
		}, cm)
		g.Expect(err).To(MatchError("configmaps \"cm2\" not found"))
	})
}

func (suite *GitopsTestSuite) doTestDelete(delete bool) {
	g := NewWithT(suite.T())

	p := test_project.NewTestProject(suite.T())
	createNamespace(suite.T(), suite.k, p.TestSlug())

	p.UpdateTarget("target1", nil)

	p.AddKustomizeDeployment("d1", []test_project.KustomizeResource{
		{Name: "cm1.yaml", Content: uo.FromStringMust(`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
  namespace: "{{ args.namespace }}"
data:
  k1: v1
`)},
	}, nil)

	key := suite.createKluctlDeployment(p, "target1", map[string]any{
		"namespace": p.TestSlug(),
	})

	suite.updateKluctlDeployment(key, func(kd *kluctlv1.KluctlDeployment) {
		kd.Spec.Delete = delete
	})

	suite.waitForCommit(key, getHeadRevision(suite.T(), p))

	cm := &corev1.ConfigMap{}

	suite.Run("cm1 got deployed", func() {
		err := suite.k.Client.Get(context.TODO(), client.ObjectKey{
			Name:      "cm1",
			Namespace: p.TestSlug(),
		}, cm)
		g.Expect(err).To(Succeed())
	})

	suite.deleteKluctlDeployment(key)

	g.Eventually(func() bool {
		var obj kluctlv1.KluctlDeployment
		err := suite.k.Client.Get(context.Background(), key, &obj)
		if err == nil {
			return false
		}
		if !errors.IsNotFound(err) {
			return false
		}
		return true
	}, timeout, time.Second).Should(BeTrue())

	if delete {
		suite.Run("cm1 was deleted", func() {
			err := suite.k.Client.Get(context.TODO(), client.ObjectKey{
				Name:      "cm1",
				Namespace: p.TestSlug(),
			}, cm)
			g.Expect(err).To(MatchError("configmaps \"cm1\" not found"))
		})
	} else {
		suite.Run("cm1 was not deleted", func() {
			err := suite.k.Client.Get(context.TODO(), client.ObjectKey{
				Name:      "cm1",
				Namespace: p.TestSlug(),
			}, cm)
			g.Expect(err).To(Succeed())
		})
	}
}

func (suite *GitopsTestSuite) Test_Delete_True() {
	suite.doTestDelete(true)
}

func (suite *GitopsTestSuite) Test_Delete_False() {
	suite.doTestDelete(false)
}

func TestGitOps(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(GitopsTestSuite))
}
