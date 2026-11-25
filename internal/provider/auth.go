package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

const (
	mountableSecretLabel = "neo4j.com/mountable-by-operator"
	allowedForLabel      = "neo4j.com/allowed-for"

	authKey = "NEO4J_AUTH"
	// authUser is the only user the Neo4j image sets a password for.
	authUser = "neo4j"
)

func authSecretName(c *controller.Context) string { return c.Name() + "-auth" }

// errUnauthorized is returned by a passwordChanger when Neo4j rejects the
// credentials it logged in with.
var errUnauthorized = errors.New("credentials rejected by Neo4j")

// boltTarget is where and how the provider reaches Neo4j over Bolt.
type boltTarget struct {
	uri     string
	rootCAs *x509.CertPool
}

// passwordChanger changes the password of a Neo4j user. Tests replace it.
type passwordChanger interface {
	// ChangePassword logs in as user with oldPassword and sets newPassword.
	ChangePassword(ctx context.Context, target boltTarget, user, oldPassword, newPassword string) error
	// Login checks that Neo4j accepts user with password.
	Login(ctx context.Context, target boltTarget, user, password string) error
}

var neo4jPasswords passwordChanger = boltPasswordChanger{}

// ensureAuthSecret keeps the operator-expected Secret holding the credentials
// Neo4j runs with. Neo4j reads NEO4J_AUTH only when it initialises the system
// database, so the user's Secret is copied as is only until Neo4j exists; a
// later change is applied by rotatePassword. The user's Secret is never
// relabeled or owned by the provider.
func ensureAuthSecret(c *controller.Context) error {
	ref := c.Instance().Spec.UserSecretRef
	if ref == nil {
		return nil
	}
	desired, err := userAuth(c)
	if err != nil {
		return err
	}
	current := &corev1.Secret{}
	if err := c.Get(current, authSecretName(c)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get Secret %q: %w", authSecretName(c), err)
	} else if err == nil && len(current.Data[authKey]) > 0 {
		live := &neo4jv1.Neo4j{}
		if err := c.Get(live, c.Name()); err == nil {
			desired = string(current.Data[authKey])
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get Neo4j: %w", err)
		}
	}
	return applyAuthSecret(c, desired)
}

// rotatePassword changes the password inside Neo4j when the user's Secret
// holds a new one, and only then hands it to the operator and to clients
// through the auth Secret. It waits until Neo4j runs; Status reports the
// change as pending meanwhile.
func rotatePassword(c *controller.Context) error {
	if c.Instance().Spec.UserSecretRef == nil {
		return nil
	}
	desired, err := userAuth(c)
	if err != nil {
		return err
	}
	current := &corev1.Secret{}
	if err := c.Get(current, authSecretName(c)); err != nil {
		return fmt.Errorf("get Secret %q: %w", authSecretName(c), err)
	}
	if string(current.Data[authKey]) == desired {
		return nil
	}
	live := &neo4jv1.Neo4j{}
	if err := c.Get(live, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get Neo4j: %w", err)
	}
	if live.Status.Phase != neo4jv1.Neo4jPhaseRunning {
		return nil
	}

	user, oldPassword, ok := strings.Cut(string(current.Data[authKey]), "/")
	if !ok {
		return fmt.Errorf("auth Secret %q has an invalid %s value", authSecretName(c), authKey)
	}
	newUser, newPassword, _ := strings.Cut(desired, "/")
	if newUser != user {
		return fmt.Errorf("the user in %s cannot change from %q to %q", authKey, user, newUser)
	}
	target, err := adminBoltTarget(c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.Context(), time.Minute)
	defer cancel()
	err = neo4jPasswords.ChangePassword(ctx, target, user, oldPassword, newPassword)
	if errors.Is(err, errUnauthorized) {
		// An earlier pass may have changed the password and failed to
		// record it.
		if loginErr := neo4jPasswords.Login(ctx, target, user, newPassword); loginErr == nil {
			err = nil
		} else if errors.Is(loginErr, errUnauthorized) {
			return fmt.Errorf("change the Neo4j password of user %q: Neo4j accepts neither the current nor the new password", user)
		}
	}
	if err != nil {
		return fmt.Errorf("change the Neo4j password of user %q: %w", user, err)
	}
	return applyAuthSecret(c, desired)
}

// passwordChangePending reports whether the user's Secret holds credentials
// Neo4j does not run with yet.
func passwordChangePending(c *controller.Context) (bool, error) {
	if c.Instance().Spec.UserSecretRef == nil {
		return false, nil
	}
	desired, err := userAuth(c)
	if err != nil {
		return false, err
	}
	current := &corev1.Secret{}
	if err := c.Get(current, authSecretName(c)); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("get Secret %q: %w", authSecretName(c), err)
	}
	return string(current.Data[authKey]) != desired, nil
}

func userAuth(c *controller.Context) (string, error) {
	ref := c.Instance().Spec.UserSecretRef
	source := &corev1.Secret{}
	if err := c.Get(source, ref.Name); err != nil {
		return "", fmt.Errorf("get user Secret %q: %w", ref.Name, err)
	}
	return string(source.Data[authKey]), nil
}

func applyAuthSecret(c *controller.Context, auth string) error {
	target := &corev1.Secret{}
	target.ObjectMeta = c.ObjectMeta(authSecretName(c))
	if target.Labels == nil {
		target.Labels = map[string]string{}
	}
	target.Labels[mountableSecretLabel] = "true"
	target.Labels[allowedForLabel] = c.Name()
	target.Data = map[string][]byte{authKey: []byte(auth)}
	return c.Apply(target)
}

// adminBoltTarget dials Neo4j the way the operator does for its admin
// connection: the client Service with routing, so that a cluster's system
// database leader takes the write, and verified TLS when Bolt has it.
func adminBoltTarget(c *controller.Context) (boltTarget, error) {
	host := fmt.Sprintf("%s.%s.svc:7687", c.Name(), c.Namespace())
	params, err := tlsParameters(c)
	if err != nil {
		return boltTarget{}, err
	}
	if !tlsEnabled(params) {
		return boltTarget{uri: "neo4j://" + host}, nil
	}
	name := tlsSecretName(c, params, "bolt")
	secret := &corev1.Secret{}
	if err := c.Get(secret, name); err != nil {
		return boltTarget{}, fmt.Errorf("get Neo4j TLS Secret %q: %w", name, err)
	}
	pool := x509.NewCertPool()
	added := false
	for _, key := range []string{"ca.crt", "tls.crt"} {
		if pool.AppendCertsFromPEM(secret.Data[key]) {
			added = true
		}
	}
	if !added {
		return boltTarget{}, fmt.Errorf("the Neo4j TLS Secret %q has no certificate to trust", name)
	}
	return boltTarget{uri: "neo4j+s://" + host, rootCAs: pool}, nil
}

type boltPasswordChanger struct{}

func (boltPasswordChanger) ChangePassword(ctx context.Context, target boltTarget, user, oldPassword, newPassword string) error {
	return runOnSystem(ctx, target, user, oldPassword,
		"ALTER CURRENT USER SET PASSWORD FROM $old TO $new",
		map[string]any{"old": oldPassword, "new": newPassword})
}

func (boltPasswordChanger) Login(ctx context.Context, target boltTarget, user, password string) error {
	return runOnSystem(ctx, target, user, password, "SHOW CURRENT USER YIELD user", nil)
}

func runOnSystem(ctx context.Context, target boltTarget, user, password, query string, params map[string]any) error {
	driver, err := neo4j.NewDriverWithContext(target.uri, neo4j.BasicAuth(user, password, ""),
		func(cfg *config.Config) {
			cfg.SocketConnectTimeout = 10 * time.Second
			if target.rootCAs != nil {
				cfg.TlsConfig = &tls.Config{RootCAs: target.rootCAs, MinVersion: tls.VersionTLS12}
			}
		})
	if err != nil {
		return err
	}
	defer func() { _ = driver.Close(ctx) }()
	_, err = neo4j.ExecuteQuery(ctx, driver, query, params, neo4j.EagerResultTransformer,
		neo4j.ExecuteQueryWithDatabase("system"))
	var neoErr *neo4j.Neo4jError
	if errors.As(err, &neoErr) && neoErr.IsAuthenticationFailed() {
		return errUnauthorized
	}
	return err
}

// enqueueUserSecretInstances enqueues the Instances whose spec.userSecretRef
// names the Secret. The runtime only reacts to a user Secret being created or
// deleted; a changed password has to start a reconcile here.
func (p *Provider) enqueueUserSecretInstances(ctx context.Context, obj client.Object) []reconcile.Request {
	if p.client == nil {
		return nil
	}
	list := &corev1alpha1.InstanceList{}
	if err := p.client.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		in := &list.Items[i]
		if in.Spec.ProviderRef.Name == p.ProviderName && in.Spec.UserSecretRef != nil &&
			in.Spec.UserSecretRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(in)})
		}
	}
	return reqs
}
