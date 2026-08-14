package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"

	configv1 "github.com/openshift/api/config/v1"
	imagev1 "github.com/openshift/api/image/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	DefaultOAuthProxyImage = "quay.io/openshift/origin-oauth-proxy:4.15"
	ACMObservabilityNS     = "open-cluster-management-observability"
)

// ClusterInfo holds auto-detected cluster topology values.
// Cached after first successful detection to avoid repeated API calls.
type ClusterInfo struct {
	// IngressDomain is the wildcard app domain, e.g. apps.cluster.example.com
	IngressDomain string
	// OAuthProxyImage is the cluster's oauth-proxy image ref
	OAuthProxyImage string
	// ACMAvailable is true when ACM observability is installed
	ACMAvailable bool
	// ACMThanosURL is the in-cluster URL of the ACM Thanos querier service,
	// e.g. http://thanos-querier.open-cluster-management-observability.svc:9091
	// Empty when ACM is not available or the service was not found.
	ACMThanosURL string
}

var (
	clusterInfoOnce   sync.Once
	cachedClusterInfo *ClusterInfo
)

// DetectClusterInfo reads live cluster configuration and returns cached results.
// Safe to call on every reconcile — detection only runs once.
func DetectClusterInfo(ctx context.Context, c client.Client) *ClusterInfo {
	clusterInfoOnce.Do(func() {
		info := &ClusterInfo{
			OAuthProxyImage: DefaultOAuthProxyImage,
		}

		// 1. Ingress domain from config.openshift.io/v1 Ingress "cluster"
		var ingressConfig configv1.Ingress
		if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, &ingressConfig); err == nil {
			info.IngressDomain = ingressConfig.Spec.Domain
		}

		// 2. oauth-proxy ImageStream tag in openshift namespace
		var is imagev1.ImageStream
		if err := c.Get(ctx, types.NamespacedName{Namespace: "openshift", Name: "oauth-proxy"}, &is); err == nil {
			for _, tag := range is.Spec.Tags {
				if tag.Name == "v4.4" || tag.Name == "latest" {
					if ref := tag.From; ref != nil && ref.Name != "" {
						info.OAuthProxyImage = ref.Name
					}
					break
				}
			}
			// Fallback: use status.dockerImageRepository + latest tag
			if is.Status.DockerImageRepository != "" && !strings.Contains(info.OAuthProxyImage, "@") {
				for _, tag := range is.Status.Tags {
					if tag.Tag == "latest" || tag.Tag == "v4.4" {
						if len(tag.Items) > 0 {
							info.OAuthProxyImage = fmt.Sprintf("%s@%s", is.Status.DockerImageRepository, tag.Items[0].Image)
							break
						}
					}
				}
			}
		}

		// 3. ACM availability — namespace existence check
		var ns corev1.Namespace
		if err := c.Get(ctx, types.NamespacedName{Name: ACMObservabilityNS}, &ns); err == nil {
			info.ACMAvailable = true

			// 4. ACM Thanos querier URL — check for the thanos-querier Service
			var thanosSvc corev1.Service
			if err := c.Get(ctx, types.NamespacedName{Namespace: ACMObservabilityNS, Name: "thanos-querier"}, &thanosSvc); err == nil {
				info.ACMThanosURL = fmt.Sprintf("http://thanos-querier.%s.svc:9091", ACMObservabilityNS)
			}
		}

		logger := log.FromContext(ctx)
		logger.Info("Cluster auto-detection complete",
			"ingressDomain", info.IngressDomain,
			"oauthProxyImage", info.OAuthProxyImage,
			"acmAvailable", info.ACMAvailable,
			"acmThanosURL", info.ACMThanosURL,
		)
		cachedClusterInfo = info
	})
	return cachedClusterInfo
}

// ResetClusterInfoCache clears the detection cache — for testing only.
func ResetClusterInfoCache() {
	clusterInfoOnce = sync.Once{}
	cachedClusterInfo = nil
}
