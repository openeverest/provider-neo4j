package provider

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

const (
	mountableSecretLabel = "neo4j.com/mountable-by-operator"
	allowedForLabel      = "neo4j.com/allowed-for"
)

func authSecretName(c *controller.Context) string { return c.Name() + "-auth" }

// ensureAuthSecret copies user-managed credentials to an operator-expected
// Secret. The user's Secret is never relabeled or owned by the provider.
func ensureAuthSecret(c *controller.Context) error {
	ref := c.Instance().Spec.UserSecretRef
	if ref == nil {
		return nil
	}
	source := &corev1.Secret{}
	if err := c.Get(source, ref.Name); err != nil {
		return fmt.Errorf("get user Secret %q: %w", ref.Name, err)
	}
	target := &corev1.Secret{}
	target.ObjectMeta = c.ObjectMeta(authSecretName(c))
	if target.Labels == nil {
		target.Labels = map[string]string{}
	}
	target.Labels[mountableSecretLabel] = "true"
	target.Labels[allowedForLabel] = c.Name()
	target.Data = map[string][]byte{"NEO4J_AUTH": append([]byte(nil), source.Data["NEO4J_AUTH"]...)}
	return c.Apply(target)
}
