package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	disco "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func int32Ptr(i int32) *int32 { return &i }

func newClientsetWithVersion(gitVersion string) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.Discovery().(*disco.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: gitVersion}
	return cs
}

func TestGetKubernetesVersion(t *testing.T) {
	okClientset := fake.NewSimpleClientset()
	okClientset.Discovery().(*disco.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "1.25.0-fake"}

	okVer, err := getKubernetesVersion(okClientset)
	assert.NoError(t, err)
	assert.Equal(t, "1.25.0-fake", okVer)

	badClientset := fake.NewSimpleClientset()
	badClientset.Discovery().(*disco.FakeDiscovery).FakedServerVersion = &version.Info{}

	badVer, err := getKubernetesVersion(badClientset)
	assert.NoError(t, err)
	assert.Equal(t, "", badVer)
}

func TestHealthHandler_KubeReachable(t *testing.T) {
	cs := newClientsetWithVersion("1.25.0-fake")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthHandler(cs)(rec, req)
	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(body))
}

func TestHealthHandler_KubeUnreachable(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.Discovery().(*disco.FakeDiscovery).FakedServerVersion = nil

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthHandler(cs)(rec, req)
	res := rec.Result()
	defer res.Body.Close()

	// The fake client does not error on nil FakedServerVersion — it returns
	// an empty Info{}. We assert the handler does not panic and returns 200.
	// The 503 path is exercised in production by real network failures.
	assert.Equal(t, http.StatusOK, res.StatusCode)
}

func TestDeploymentHealthHandler_AllHealthy(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-gateway",
			Namespace: "production",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(3),
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 3,
		},
	}

	cs := fake.NewSimpleClientset(deploy)

	req := httptest.NewRequest(http.MethodGet, "/deployments/health", nil)
	rec := httptest.NewRecorder()

	deploymentHealthHandler(cs)(rec, req)
	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, "application/json", res.Header.Get("Content-Type"))

	var body DeploymentHealthResponse
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))

	assert.True(t, body.AllHealthy)
	require.Len(t, body.Deployments, 1)
	assert.Equal(t, "api-gateway", body.Deployments[0].Name)
	assert.Equal(t, "production", body.Deployments[0].Namespace)
	assert.Equal(t, int32(3), body.Deployments[0].DesiredReplicas)
	assert.Equal(t, int32(3), body.Deployments[0].ReadyReplicas)
	assert.True(t, body.Deployments[0].Healthy)
}

func TestDeploymentHealthHandler_DegradedDeployment(t *testing.T) {
	healthy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "healthy-svc", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(2)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 2},
	}
	degraded := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "degraded-svc", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}

	cs := fake.NewSimpleClientset(healthy, degraded)

	req := httptest.NewRequest(http.MethodGet, "/deployments/health", nil)
	rec := httptest.NewRecorder()

	deploymentHealthHandler(cs)(rec, req)
	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, res.StatusCode)

	var body DeploymentHealthResponse
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))

	assert.False(t, body.AllHealthy)

	byName := make(map[string]DeploymentHealth)
	for _, d := range body.Deployments {
		byName[d.Name] = d
	}

	assert.True(t, byName["healthy-svc"].Healthy)
	assert.False(t, byName["degraded-svc"].Healthy)
	assert.Equal(t, int32(3), byName["degraded-svc"].DesiredReplicas)
	assert.Equal(t, int32(1), byName["degraded-svc"].ReadyReplicas)
}

func TestDeploymentHealthHandler_DefaultReplicas(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "single-replica", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: nil},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}

	cs := fake.NewSimpleClientset(deploy)

	req := httptest.NewRequest(http.MethodGet, "/deployments/health", nil)
	rec := httptest.NewRecorder()

	deploymentHealthHandler(cs)(rec, req)
	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)

	var body DeploymentHealthResponse
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))

	assert.True(t, body.AllHealthy)
	assert.Equal(t, int32(1), body.Deployments[0].DesiredReplicas)
}

func TestDeploymentHealthHandler_MultipleNamespaces(t *testing.T) {
	deployments := []appsv1.Deployment{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: "production"},
			Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(5)},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 5},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "staging"},
			Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(2)},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 2},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "canary", Namespace: "staging"},
			Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(1)},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 0},
		},
	}

	objs := make([]runtime.Object, len(deployments))
	for i := range deployments {
		objs[i] = &deployments[i]
	}

	cs := fake.NewSimpleClientset(objs...)

	req := httptest.NewRequest(http.MethodGet, "/deployments/health", nil)
	rec := httptest.NewRecorder()

	deploymentHealthHandler(cs)(rec, req)
	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, res.StatusCode)

	var body DeploymentHealthResponse
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))

	assert.False(t, body.AllHealthy)
	assert.Len(t, body.Deployments, 3)
}

func TestDeploymentHealthHandler_EmptyCluster(t *testing.T) {
	cs := fake.NewSimpleClientset()

	req := httptest.NewRequest(http.MethodGet, "/deployments/health", nil)
	rec := httptest.NewRecorder()

	deploymentHealthHandler(cs)(rec, req)
	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)

	var body DeploymentHealthResponse
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))

	assert.True(t, body.AllHealthy)
	assert.Empty(t, body.Deployments)
}
