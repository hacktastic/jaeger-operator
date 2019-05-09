// +build smoke

package e2e

import (
	goctx "context"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	osv1sec "github.com/openshift/api/security/v1"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/operator-framework/operator-sdk/pkg/test/e2eutil"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/jaegertracing/jaeger-operator/pkg/apis/jaegertracing/v1"
)



type DaemonSetTestSuite struct {
	suite.Suite
}

func(suite *DaemonSetTestSuite) SetupSuite() {
	t = suite.T()
	var err error
	ctx, err = prepare(t)
	if (err != nil) {
		ctx.Cleanup()
		require.FailNow(t, "Failed in prepare")
	}
	fw = framework.Global
	namespace, _ = ctx.GetNamespace()
	require.NotNil(t, namespace, "GetNamespace failed")

	addToFrameworkSchemeForSmokeTests(t)
}

func (suite *DaemonSetTestSuite) TearDownSuite() {
	log.Info("Entering TearDownSuite()")
	ctx.Cleanup()
}

func TestDaemonSetSuite(t *testing.T) {
	suite.Run(t, new(DaemonSetTestSuite))
}

// DaemonSet runs a test with the agent as DaemonSet
func (suite *DaemonSetTestSuite) TestDaemonSet()  {
	cleanupOptions := &framework.CleanupOptions{TestContext: ctx, Timeout: timeout, RetryInterval: retryInterval}

	if isOpenShift(t) {
		err := fw.Client.Create(goctx.TODO(), hostPortSccDaemonset(), cleanupOptions)
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("Failed creating hostPortSccDaemonset %v\n", err)
		}

		// ideally, we would use the REST API, but for a single-usage within the project, this is the simplest solution that works
		cmd := exec.Command("oc", "adm", "--namespace", namespace, "policy",  "add-scc-to-user", "daemonset-with-hostport", "-z", "default")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err,"Failed creating hostport scc with OUTPUT: [%s]\n", string(output) )
	}

	j := jaegerAgentAsDaemonsetDefinition(namespace, "agent-as-daemonset")
	log.Infof("passing %v", j)
	err := fw.Client.Create(goctx.TODO(), j, cleanupOptions)
	require.NoError(t, err, "Error deploying jaeger")

	err = WaitForDaemonSet(t, fw.KubeClient, namespace, "agent-as-daemonset-agent-daemonset", retryInterval, timeout)
	require.NoError(t, err, "Error waiting for daemonset to startup")

	selector := map[string]string{"app": "vertx-create-span"}
	dep := getVertxDeployment(namespace, selector)
	err = fw.Client.Create(goctx.TODO(), dep, cleanupOptions)
	require.NoError(t, err, "Error creating VertX app")

	err = e2eutil.WaitForDeployment(t, fw.KubeClient, namespace, "vertx-create-span", 1, retryInterval, timeout)
	require.NoError(t, err, "Error waiting for VertX app to start")

	queryPod, err := GetPod(namespace, "agent-as-daemonset", "jaegertracing/all-in-one", fw.KubeClient)
	require.NoErrorf(t, err, "Error trying to find pod with prefix agent-as-daemonset and image jaegertracing/all-in-one in namespace [%s]: %s\n", namespace)

	portForw, closeChan, err := CreatePortForward(namespace, queryPod.Name, []string{"16686"}, fw.KubeConfig)
	require.NoError(t, err, "Error creating portforward")
	defer portForw.Close()
	defer close(closeChan)

	url := "http://localhost:16686/api/traces?service=order"
	c := http.Client{Timeout: time.Second}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err, "Failed to create httpRequest")

	err =  wait.Poll(retryInterval, timeout, func() (done bool, err error) {
		res, err := c.Do(req)
		if err != nil {
			return false, err
		}

		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return false, err
		}

		resp := &resp{}
		err = json.Unmarshal(body, &resp)
		if err != nil {
			return false, err
		}

		return len(resp.Data) > 0, nil
	})
	require.NoError(t, err, "Failed waiting for expected content")
}

func getVertxDeployment(namespace string, selector map[string]string) *appsv1.Deployment {
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vertx-create-span",
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: selector,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: selector,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Image: "jaegertracing/vertx-create-span:operator-e2e-tests",
						Name:  "vertx-create-span",
						Env: []corev1.EnvVar{
							corev1.EnvVar{
								Name: "JAEGER_AGENT_HOST",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "status.hostIP",
									},
								},
							},
						},
						Ports: []corev1.ContainerPort{
							{
								ContainerPort: 8080,
							},
						},
						ReadinessProbe: &corev1.Probe{
							Handler: corev1.Handler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/",
									Port: intstr.FromInt(8080),
								},
							},
							InitialDelaySeconds: 1,
						},
						LivenessProbe: &corev1.Probe{
							Handler: corev1.Handler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/",
									Port: intstr.FromInt(8080),
								},
							},
							InitialDelaySeconds: 1,
						},
					}},
				},
			},
		},
	}
	return dep
}

func jaegerAgentAsDaemonsetDefinition(namespace string, name string) *v1.Jaeger {
	j := &v1.Jaeger{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Jaeger",
			APIVersion: "jaegertracing.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1.JaegerSpec{
			Strategy: "allInOne",
			AllInOne: v1.JaegerAllInOneSpec{},
			Agent: v1.JaegerAgentSpec{
				Strategy: "DaemonSet",
				Options: v1.NewOptions(map[string]interface{}{
					"log-level": "debug",
				}),
			},
		},
	}
	return j
}

func hostPortSccDaemonset() (*osv1sec.SecurityContextConstraints) {
	annotations := make(map[string]string)
	annotations["kubernetes.io/description"] = "Allows DaemonSets to bind to a well-known host port"

	scc := &osv1sec.SecurityContextConstraints{
		TypeMeta: metav1.TypeMeta {
			Kind: "SecurityContextConstraints",
			APIVersion:"security.openshift.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta {
			Name: "daemonset-with-hostport",
			Annotations:annotations,
		},
		RunAsUser: osv1sec.RunAsUserStrategyOptions{
			Type: osv1sec.RunAsUserStrategyRunAsAny,
		},
		SELinuxContext: osv1sec.SELinuxContextStrategyOptions{
			Type:"RunAsAny",
		},
		AllowHostPorts: true,
	}
	return scc
}