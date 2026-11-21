package provider

import (
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/definition"
	"github.com/openeverest/provider-neo4j/definition/components"
	"github.com/openeverest/provider-neo4j/internal/common"
)

var (
	minStorage = resource.MustParse("1Gi")
	minCPU     = resource.MustParse("500m")
	minMemory  = resource.MustParse("2Gi")
)

func (p *Provider) Validate(in controller.ValidationInput) controller.ValidationResult {
	var params definition.Parameters
	_ = decodeRaw(in.Instance.Spec.Parameters, &params)
	edition := effectiveEdition(params)
	errs := validateEdition(in.Instance, edition, params)
	errs = append(errs, validateTLS(in.Instance, params)...)
	errs = append(errs, validateComponents(in.Instance, edition)...)
	errs = append(errs, validateDataSource(in.Instance)...)
	errs = append(errs, validateTransitions(in.Instance, in.Old, params)...)
	return controller.ValidationResult{Errors: errs}
}

func (p *Provider) ValidateReferences(c *controller.Context) (field.ErrorList, error) {
	var errs field.ErrorList
	authErrs, err := validateUserSecret(c)
	if err != nil {
		return nil, err
	}
	errs = append(errs, authErrs...)
	tlsErrs, err := validateTLSReferences(c)
	if err != nil {
		return nil, err
	}
	return append(errs, tlsErrs...), nil
}

func decodeRaw(raw *runtime.RawExtension, target any) error {
	if raw == nil || len(raw.Raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw.Raw, target)
}

func validateEdition(in *corev1alpha1.Instance, edition definition.Edition, params definition.Parameters) field.ErrorList {
	path := field.NewPath("spec", "parameters")
	var errs field.ErrorList
	switch edition {
	case definition.EditionCommunity:
		if in.GetTopologyType() != common.TopologyStandalone {
			errs = append(errs, field.Forbidden(path.Child("edition"), "Community edition supports only the standalone topology"))
		}
		if params.LicenseAcceptance != "" {
			errs = append(errs, field.Forbidden(path.Child("licenseAcceptance"), "license acceptance applies only to Enterprise edition"))
		}
	case definition.EditionEnterprise:
		if !slices.Contains([]definition.LicenseAcceptance{
			definition.LicenseAcceptanceYes, definition.LicenseAcceptanceEval,
		}, params.LicenseAcceptance) {
			errs = append(errs, field.Required(path.Child("licenseAcceptance"),
				`set "yes" for production or "eval" for evaluation`,
			))
		}
	default:
		errs = append(errs, field.NotSupported(path.Child("edition"), edition, []definition.Edition{
			definition.EditionCommunity, definition.EditionEnterprise,
		}))
	}
	if edition == definition.EditionCommunity && in.Spec.Backup != nil && in.Spec.Backup.Enabled {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "backup", "enabled"),
			"online backup is an Enterprise feature"))
	}
	if edition == definition.EditionCommunity && in.GetMetricsSourcePolicy() == corev1alpha1.MonitoringSourcePolicyAlways {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "monitoring", "sources", "metrics", "policy"),
			"Prometheus metrics are an Enterprise feature"))
	}
	return errs
}

func validateComponents(in *corev1alpha1.Instance, edition definition.Edition) field.ErrorList {
	base := field.NewPath("spec", "components")
	engine := in.Spec.Components[common.ComponentEngine]
	path := base.Key(common.ComponentEngine)
	var errs field.ErrorList
	if engine.Storage == nil {
		errs = append(errs, field.Required(path.Child("storage"), "data storage is required"))
	} else if engine.Storage.Size.Cmp(minStorage) < 0 {
		errs = append(errs, field.Invalid(path.Child("storage", "size"), engine.Storage.Size.String(),
			"must be at least "+minStorage.String()))
	}
	errs = append(errs, validateResources(path, engine.Resources)...)
	if engine.Service != nil {
		switch engine.Service.ServiceType {
		case corev1.ServiceTypeClusterIP, corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:
		default:
			errs = append(errs, field.NotSupported(path.Child("service", "serviceType"), engine.Service.ServiceType,
				[]corev1.ServiceType{corev1.ServiceTypeClusterIP, corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer}))
		}
		if lb := engine.Service.LoadBalancerService; lb != nil {
			for i, r := range lb.SourceRanges {
				if _, _, err := net.ParseCIDR(r); err != nil && net.ParseIP(r) == nil {
					errs = append(errs, field.Invalid(path.Child("service", "loadBalancerService", "sourceRanges").Index(i), r,
						"must be an IP address or a CIDR"))
				}
			}
		}
	}
	switch in.GetTopologyType() {
	case common.TopologyStandalone:
		if engine.Replicas != nil && *engine.Replicas != 1 {
			errs = append(errs, field.Invalid(path.Child("replicas"), *engine.Replicas, "must be 1 for Standalone"))
		}
	case common.TopologyCluster:
		replicas := replicaCount(engine, 3)
		if replicas < 1 || (replicas > 1 && replicas%2 == 0) {
			errs = append(errs, field.Invalid(path.Child("replicas"), replicas,
				"must be 1 or a positive odd number"))
		}
		if params := decodeComponentParams(engine); slices.ContainsFunc(params.Plugins, licensedPrimaryPlugin) {
			errs = append(errs, field.Forbidden(path.Child("parameters", "plugins"),
				"GDS and Bloom must be installed on an analytics or read pool in Cluster mode"))
		}
		for _, name := range []string{common.ComponentAnalytics, common.ComponentRead} {
			if component, ok := in.Spec.Components[name]; ok {
				poolPath := base.Key(name)
				if component.Replicas == nil || *component.Replicas < 1 {
					errs = append(errs, field.Invalid(poolPath.Child("replicas"), component.Replicas, "must be at least 1"))
				}
				errs = append(errs, validateOptionalResources(poolPath, component.Resources)...)
			}
		}
	}
	_ = edition
	return errs
}

func licensedPrimaryPlugin(plugin string) bool { return plugin == "gds" || plugin == "bloom" }

func decodeComponentParams(component corev1alpha1.ComponentSpec) components.Neo4jParameters {
	var out components.Neo4jParameters
	_ = decodeRaw(component.Parameters, &out)
	return out
}

func validateResources(path *field.Path, resources *corev1.ResourceRequirements) field.ErrorList {
	limits := (*corev1.ResourceList)(nil)
	if resources != nil {
		limits = &resources.Limits
	}
	return validateLimits(path.Child("resources", "limits"), limits)
}

func validateOptionalResources(path *field.Path, resources *corev1.ResourceRequirements) field.ErrorList {
	if resources == nil {
		return nil
	}
	return validateLimits(path.Child("resources", "limits"), &resources.Limits)
}

func validateLimits(path *field.Path, limits *corev1.ResourceList) field.ErrorList {
	var errs field.ErrorList
	if limits == nil || limits.Cpu().IsZero() {
		errs = append(errs, field.Required(path.Key("cpu"), "must be at least "+minCPU.String()))
	} else if limits.Cpu().Cmp(minCPU) < 0 {
		errs = append(errs, field.Invalid(path.Key("cpu"), limits.Cpu().String(), "must be at least "+minCPU.String()))
	}
	if limits == nil || limits.Memory().IsZero() {
		errs = append(errs, field.Required(path.Key("memory"), "must be at least "+minMemory.String()))
	} else if limits.Memory().Cmp(minMemory) < 0 {
		errs = append(errs, field.Invalid(path.Key("memory"), limits.Memory().String(), "must be at least "+minMemory.String()))
	}
	return errs
}

func validateDataSource(in *corev1alpha1.Instance) field.ErrorList {
	if in.Spec.DataSource == nil {
		return nil
	}

	if in.Spec.DataSource.Type == backupv1alpha1.DataSourceTypePointInTime {
		return field.ErrorList{field.NotSupported(field.NewPath("spec", "dataSource", "type"),
			in.Spec.DataSource.Type, []backupv1alpha1.DataSourceType{backupv1alpha1.DataSourceTypeBackup})}
	}
	return nil
}

func validateTransitions(in, old *corev1alpha1.Instance, params definition.Parameters) field.ErrorList {
	if old == nil {
		return nil
	}
	var errs field.ErrorList
	if old.GetTopologyType() != in.GetTopologyType() && old.GetTopologyType() != "" {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "topology", "type"),
			"Neo4j topology mode cannot change after creation"))
	}
	oldParams := definition.Parameters{}
	_ = decodeRaw(old.Spec.Parameters, &oldParams)
	oldTLS := definition.TLSParameters{}
	if oldParams.TLS != nil {
		oldTLS = *oldParams.TLS
	}
	newTLS := definition.TLSParameters{}
	if params.TLS != nil {
		newTLS = *params.TLS
	}
	if oldTLS.Mode == "" {
		oldTLS.Mode = definition.TLSModeEnabled
	}
	if newTLS.Mode == "" {
		newTLS.Mode = definition.TLSModeEnabled
	}
	if oldTLS.Mode != newTLS.Mode {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "parameters", "tls", "mode"),
			"TLS cannot be enabled or disabled on an existing instance"))
	}
	oldSource := oldTLS.CertificateSecretName
	newSource := newTLS.CertificateSecretName
	if oldTLS.Issuer != nil {
		oldSource = string(oldTLS.Issuer.Kind) + "/" + oldTLS.Issuer.Name
	}
	if newTLS.Issuer != nil {
		newSource = string(newTLS.Issuer.Kind) + "/" + newTLS.Issuer.Name
	}
	if oldSource != newSource {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "parameters", "tls"),
			"the certificate source cannot change on an existing instance"))
	}
	return errs
}

func validateUserSecret(c *controller.Context) (field.ErrorList, error) {
	ref := c.Instance().Spec.UserSecretRef
	if ref == nil {
		return nil, nil
	}
	path := field.NewPath("spec", "userSecretRef", "name")
	secret := &corev1.Secret{}
	if err := c.Get(secret, ref.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return field.ErrorList{field.NotFound(path, ref.Name)}, nil
		}
		return nil, fmt.Errorf("get user Secret %q: %w", ref.Name, err)
	}
	spec, err := c.ProviderSpec()
	if err != nil {
		return nil, fmt.Errorf("get Provider spec: %w", err)
	}
	if err := controller.ValidateSecretSchema(secret, spec); err != nil {
		return field.ErrorList{field.Invalid(path, ref.Name, err.Error())}, nil
	}
	auth := string(secret.Data["NEO4J_AUTH"])
	user, password, ok := strings.Cut(auth, "/")
	if !ok || user == "" || password == "" {
		return field.ErrorList{field.Invalid(path, ref.Name,
			"NEO4J_AUTH must contain a non-empty username/password pair")}, nil
	}
	return nil, nil
}
