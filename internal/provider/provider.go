package provider

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/definition/backupclasses"
	"github.com/openeverest/provider-neo4j/internal/common"
)

var _ controller.ProviderInterface = (*Provider)(nil)
var _ controller.ReferenceValidator = (*Provider)(nil)
var _ controller.WatchProvider = (*Provider)(nil)
var _ controller.MonitoringSourcesProvider = (*Provider)(nil)

// Provider translates OpenEverest Instances to Neo4j operator resources.
type Provider struct {
	controller.BaseProvider

	client client.Client
}

func New() *Provider {
	p := &Provider{BaseProvider: controller.BaseProvider{
		ProviderName: common.ProviderName,
		SchemeFuncs: []func(*runtime.Scheme) error{
			neo4jv1.AddToScheme,
			cmapi.AddToScheme,
			appsv1.AddToScheme,
		},
		WatchConfigs: []controller.WatchConfig{
			controller.WatchOwned(&neo4jv1.Neo4j{}),
			controller.WatchExternal(&backupRestoreType{}, handler.EnqueueRequestsFromMapFunc(enqueueRestoreInstance())),
			controller.WatchExternal(&appsv1.StatefulSet{}, handler.EnqueueRequestsFromMapFunc(enqueueStatefulSetInstance())),
		},
	}}
	p.WatchConfigs = append(p.WatchConfigs,
		controller.WatchExternal(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(p.enqueueUserSecretInstances)))
	// Runtime-generated BackupClasses are stamped by the provider release and
	// installed with the chart.
	p.SetShippedBackupClasses(backupclasses.Shipped())
	return p
}

func (p *Provider) Sync(c *controller.Context) error {
	if err := ensureAuthSecret(c); err != nil {
		return err
	}
	if err := ensureTLSIssuer(c); err != nil {
		return err
	}
	neo4j, err := buildNeo4j(c)
	if err != nil {
		return err
	}
	if err := approveAdditionalNames(c, neo4j); err != nil {
		return err
	}
	apply, err := holdProviderUpgrade(c, neo4j)
	if err != nil {
		return err
	}
	if apply {
		if err := c.Apply(neo4j); err != nil {
			return fmt.Errorf("apply Neo4j: %w", err)
		}
	}
	if err := reconcileBackupSchedules(c, neo4j); err != nil {
		return err
	}
	if err := labelPods(c); err != nil {
		return err
	}
	if err := rotatePassword(c); err != nil {
		return err
	}
	if c.Instance().Spec.DataSource != nil {
		live := &neo4jv1.Neo4j{}
		if err := c.Get(live, c.Name()); err != nil {
			if apierrors.IsNotFound(err) {
				c.SetDataSourceStatus(controller.DataSourceStatus{
					Done: false, State: controller.DataSourceStateWaiting,
					Reason:  corev1alpha1.ReasonDataSourceWaitingForCluster,
					Message: "waiting for Neo4j to be created",
				})
				return nil
			}
			return fmt.Errorf("get live Neo4j for data source: %w", err)
		}
		if live.Status.Phase != neo4jv1.Neo4jPhaseRunning {
			c.SetDataSourceStatus(controller.DataSourceStatus{
				Done: false, State: controller.DataSourceStateWaiting,
				Reason:  corev1alpha1.ReasonDataSourceWaitingForCluster,
				Message: "waiting for Neo4j to be Ready before restoring the data source",
			})
			return nil
		}
		if _, err := c.ReconcileDataSource(); err != nil {
			return fmt.Errorf("reconcile data source: %w", err)
		}
	}
	return nil
}

// Status reports facts about Neo4j; see status.go.
func (p *Provider) Status(c *controller.Context) (controller.Status, error) {
	return status(c)
}

func connectionDetails(c *controller.Context, neo4j *neo4jv1.Neo4j) (controller.ConnectionDetails, error) {
	secretName := c.Name() + "-auth"
	if neo4j.Status.Credentials != nil && neo4j.Status.Credentials.SecretName != "" {
		secretName = neo4j.Status.Credentials.SecretName
	}
	secret := &corev1.Secret{}
	if err := c.Get(secret, secretName); err != nil {
		return controller.ConnectionDetails{}, fmt.Errorf("get Neo4j credentials Secret %q: %w", secretName, err)
	}
	auth := string(secret.Data["NEO4J_AUTH"])
	username, password, ok := strings.Cut(auth, "/")
	if !ok || username == "" || password == "" {
		return controller.ConnectionDetails{}, fmt.Errorf("credentials Secret %q has an invalid NEO4J_AUTH value", secretName)
	}

	host := c.Name()
	uri := ""
	if neo4j.Status.Endpoints != nil {
		uri = neo4j.Status.Endpoints.Neo4j
	}
	if uri == "" {
		uri = "neo4j://" + host + ":7687"
	}
	parsedURI, err := url.Parse(uri)
	if err != nil {
		return controller.ConnectionDetails{}, fmt.Errorf("parse Neo4j endpoint %q: %w", uri, err)
	}
	parsedURI.User = url.UserPassword(username, password)
	uri = parsedURI.String()
	if parsedURI.Hostname() != "" {
		host = parsedURI.Hostname()
	}
	port := parsedURI.Port()
	if port == "" {
		port = "7687"
	}

	details := controller.ConnectionDetails{
		Type:     "neo4j",
		Host:     host,
		Port:     port,
		Username: username,
		Password: password,
		URI:      uri,
		AdditionalProperties: map[string]string{
			"neo4j-auth": auth,
		},
	}
	if tls, err := tlsParameters(c); err != nil {
		return controller.ConnectionDetails{}, err
	} else if tlsEnabled(tls) {
		name := tlsSecretName(c, tls, "bolt")
		cert := &corev1.Secret{}
		if err := c.Get(cert, name); err != nil && !apierrors.IsNotFound(err) {
			return controller.ConnectionDetails{}, fmt.Errorf("get Neo4j TLS Secret %q: %w", name, err)
		} else if err == nil {
			details.AdditionalProperties["ca.crt"] = string(cert.Data["ca.crt"])
		}
	}
	return details, nil
}

func (p *Provider) Cleanup(c *controller.Context) error {
	neo4j := &neo4jv1.Neo4j{ObjectMeta: c.ObjectMeta(c.Name())}
	if err := c.Delete(neo4j); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Neo4j: %w", err)
	}
	return nil
}

// MonitoringSources exposes Neo4j's Enterprise Prometheus endpoint on every
// pool's pods.
func (p *Provider) MonitoringSources(c *controller.Context) (*corev1alpha1.MonitoringSources, error) {
	live := &neo4jv1.Neo4j{}
	if err := c.Get(live, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return &corev1alpha1.MonitoringSources{}, nil
		}
		return nil, fmt.Errorf("get Neo4j: %w", err)
	}
	out := &corev1alpha1.MonitoringSources{}
	if live.Spec.Features == nil || live.Spec.Features.Monitoring == nil ||
		live.Spec.Features.Monitoring.Prometheus == nil ||
		!live.Spec.Features.Monitoring.Prometheus.Enabled {
		return out, nil
	}
	components := slices.Sorted(maps.Keys(componentPools(c)))
	for _, component := range components {
		out.Metrics = append(out.Metrics, corev1alpha1.MetricsEndpoint{
			Component: component,
			Kind:      common.ComponentTypeNeo4j,
			// labelPods puts these labels on the pods of each pool.
			PodSelector: map[string]string{
				controller.InstanceLabel:  c.Name(),
				controller.ComponentLabel: component,
			},
			Port:   intstr.FromString("tcp-prometheus"),
			Path:   "/metrics",
			Scheme: "http",
		})
	}
	return out, nil
}
