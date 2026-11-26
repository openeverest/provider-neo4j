package provider

import (
	"context"
	"errors"
	"testing"

	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/definition"
	"github.com/openeverest/provider-neo4j/internal/common"
)

// fakePasswords is a Neo4j that accepts one password for user neo4j.
type fakePasswords struct {
	password  string
	changeErr error
	changes   []string
}

func (f *fakePasswords) ChangePassword(_ context.Context, _ boltTarget, user, oldPassword, newPassword string) error {
	if user != authUser || oldPassword != f.password {
		return errUnauthorized
	}
	if f.changeErr != nil {
		return f.changeErr
	}
	f.changes = append(f.changes, oldPassword+"->"+newPassword)
	f.password = newPassword
	return nil
}

func (f *fakePasswords) Login(_ context.Context, _ boltTarget, user, password string) error {
	if user != authUser || password != f.password {
		return errUnauthorized
	}
	return nil
}

func useFakePasswords(t *testing.T, password string) *fakePasswords {
	t.Helper()
	fake := &fakePasswords{password: password}
	previous := neo4jPasswords
	neo4jPasswords = fake
	t.Cleanup(func() { neo4jPasswords = previous })
	return fake
}

func credentialsInstance(t *testing.T) *corev1alpha1.Instance {
	t.Helper()
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "db"},
		Spec: corev1alpha1.InstanceSpec{
			ProviderRef:   commonv1alpha1.ObjectRef{Name: common.ProviderName},
			UserSecretRef: &commonv1alpha1.SecretRef{Name: "graph-user"},
			Parameters: rawExtension(t, definition.Parameters{
				TLS: &definition.TLSParameters{Mode: definition.TLSModeDisabled},
			}),
		},
	}
}

func authSecret(name, auth string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "db"},
		Data:       map[string][]byte{authKey: []byte(auth)},
	}
}

func neo4jInPhase(phase neo4jv1.Neo4jPhase) *neo4jv1.Neo4j {
	return &neo4jv1.Neo4j{
		ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "db"},
		Status:     neo4jv1.Neo4jStatus{Phase: phase},
	}
}

func storedAuth(t *testing.T, c *controller.Context) string {
	t.Helper()
	secret := &corev1.Secret{}
	require.NoError(t, c.Get(secret, "graph-auth"))
	return string(secret.Data[authKey])
}

func TestEnsureAuthSecretCopiesTheUserSecretUntilNeo4jExists(t *testing.T) {
	c := testContext(t, credentialsInstance(t),
		authSecret("graph-user", "neo4j/second"), authSecret("graph-auth", "neo4j/first"))

	require.NoError(t, ensureAuthSecret(c))

	secret := &corev1.Secret{}
	require.NoError(t, c.Get(secret, "graph-auth"))
	assert.Equal(t, "neo4j/second", string(secret.Data[authKey]))
	assert.Equal(t, "true", secret.Labels[mountableSecretLabel])
	assert.Equal(t, "graph", secret.Labels[allowedForLabel])
}

func TestEnsureAuthSecretKeepsThePasswordNeo4jRunsWith(t *testing.T) {
	c := testContext(t, credentialsInstance(t), neo4jInPhase(neo4jv1.Neo4jPhaseRunning),
		authSecret("graph-user", "neo4j/second"), authSecret("graph-auth", "neo4j/first"))

	require.NoError(t, ensureAuthSecret(c))

	assert.Equal(t, "neo4j/first", storedAuth(t, c))
}

func TestRotatePassword(t *testing.T) {
	tests := map[string]struct {
		phase      neo4jv1.Neo4jPhase
		neo4jHas   string
		changeErr  error
		wantErr    string
		wantAuth   string
		wantChange []string
	}{
		"changes the password, then records it": {
			phase: neo4jv1.Neo4jPhaseRunning, neo4jHas: "first",
			wantAuth: "neo4j/second", wantChange: []string{"first->second"},
		},
		"waits until Neo4j runs": {
			phase: "Pending", neo4jHas: "first",
			wantAuth: "neo4j/first",
		},
		"keeps the old password when Neo4j refuses the change": {
			phase: neo4jv1.Neo4jPhaseRunning, neo4jHas: "first", changeErr: errors.New("password too short"),
			wantErr: "password too short", wantAuth: "neo4j/first",
		},
		"records a change an earlier pass made": {
			phase: neo4jv1.Neo4jPhaseRunning, neo4jHas: "second",
			wantAuth: "neo4j/second",
		},
		"fails when Neo4j accepts neither password": {
			phase: neo4jv1.Neo4jPhaseRunning, neo4jHas: "third",
			wantErr: "accepts neither", wantAuth: "neo4j/first",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := useFakePasswords(t, tt.neo4jHas)
			fake.changeErr = tt.changeErr
			c := testContext(t, credentialsInstance(t), neo4jInPhase(tt.phase),
				authSecret("graph-user", "neo4j/second"), authSecret("graph-auth", "neo4j/first"))

			err := rotatePassword(c)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantAuth, storedAuth(t, c))
			assert.Equal(t, tt.wantChange, fake.changes)
		})
	}
}

func TestRotatePasswordIsANoOpWithoutChange(t *testing.T) {
	fake := useFakePasswords(t, "first")
	objects := []client.Object{neo4jInPhase(neo4jv1.Neo4jPhaseRunning),
		authSecret("graph-user", "neo4j/first"), authSecret("graph-auth", "neo4j/first")}
	c := testContext(t, credentialsInstance(t), objects...)

	require.NoError(t, rotatePassword(c))
	assert.Empty(t, fake.changes)
}

func TestInvalidAuth(t *testing.T) {
	assert.Empty(t, invalidAuth("neo4j/s3cret-pass"))
	for auth, want := range map[string]string{
		"neo4j":            "username/password pair",
		"admin/s3cret":     `must name user "neo4j"`,
		"neo4j/a/b":        `must not contain "/"`,
		"neo4j/-s3cret":    `must not start with "-"`,
		"neo4j/neo4j":      `must not be "neo4j"`,
		"neo4j/":           "username/password pair",
		"/s3cret-password": "username/password pair",
	} {
		assert.Contains(t, invalidAuth(auth), want, auth)
	}
}

func TestEnqueueUserSecretInstances(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	uses := credentialsInstance(t)
	other := credentialsInstance(t)
	other.Name = "other"
	other.Spec.UserSecretRef = &commonv1alpha1.SecretRef{Name: "other-user"}
	noRef := credentialsInstance(t)
	noRef.Name = "no-ref"
	noRef.Spec.UserSecretRef = nil
	foreign := credentialsInstance(t)
	foreign.Name = "foreign"
	foreign.Spec.ProviderRef.Name = "other-provider"
	elsewhere := credentialsInstance(t)
	elsewhere.Namespace = "elsewhere"

	p := New()
	p.SetClient(fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(uses, other, noRef, foreign, elsewhere).Build())

	reqs := p.enqueueUserSecretInstances(context.Background(), authSecret("graph-user", "neo4j/x"))
	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "db", Name: "graph"}}}, reqs)
}
